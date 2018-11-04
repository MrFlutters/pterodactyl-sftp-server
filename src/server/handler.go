package server

import (
	"github.com/pkg/errors"
	"github.com/pkg/sftp"
	"github.com/pterodactyl/sftp-server/src/logger"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

type FileSystem struct {
	directory   string
	uuid        string
	permissions []string
	readOnly    bool
	lock        sync.Mutex
}

// Creates a new SFTP handler for a given server. The directory argument should
// be the base directory for a server. All actions done on the server will be
// relative to that directory, and the user will not be able to escape out of it.
func CreateHandler(base string, perm *ssh.Permissions, ro bool) sftp.Handlers {
	p := FileSystem{
		directory:   path.Join(base, perm.Extensions["uuid"]),
		uuid:        perm.Extensions["uuid"],
		permissions: strings.Split(perm.Extensions["permissions"], ","),
		readOnly:    ro,
	}

	return sftp.Handlers{
		FileGet:  p,
		FilePut:  p,
		FileCmd:  p,
		FileList: p,
	}
}

// Creates a reader for a file on the system and returns the reader back.
func (fs FileSystem) Fileread(request *sftp.Request) (io.ReaderAt, error) {
	// Check first if the user can actually open and view a file. This permission is named
	// really poorly, but it is checking if they can read. There is an addition permission,
	// "save-files" which determines if they can write that file.
	if !fs.can("edit-files") {
		return nil, sftp.ErrSshFxPermissionDenied
	}

	p, err := fs.buildPath(request.Filepath)
	if err != nil {
		return nil, sftp.ErrSshFxNoSuchFile
	}

	fs.lock.Lock()
	defer fs.lock.Unlock()

	file, err := os.OpenFile(p, os.O_RDONLY, 0644)
	if err == os.ErrNotExist {
		return nil, sftp.ErrSshFxNoSuchFile
	} else if err != nil {
		logger.Get().Errorw("could not open file for reading", zap.String("source", p), zap.Error(err))
		return nil, sftp.ErrSshFxFailure
	}

	return file, nil
}

// Handle a write action for a file on the system.
func (fs FileSystem) Filewrite(request *sftp.Request) (io.WriterAt, error) {
	if fs.readOnly {
		return nil, sftp.ErrSshFxOpUnsupported
	}

	p, err := fs.buildPath(request.Filepath)
	if err != nil {
		return nil, sftp.ErrSshFxNoSuchFile
	}

	fs.lock.Lock()
	defer fs.lock.Unlock()

	_, statErr := os.Stat(p)
	// If the file doesn't exist we need to create it, as well as the directory pathway
	// leading up to where that file will be created.
	if os.IsNotExist(statErr) {
		// This is a different pathway than just editing an existing file. If it doesn't exist already
		// we need to determine if this user has permission to create files.
		if !fs.can("create-files") {
			return nil, sftp.ErrSshFxPermissionDenied
		}

		// Create all of the directories leading up to the location where this file is being created.
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			logger.Get().Errorw("error making path for file",
				zap.String("source", p),
				zap.String("path", filepath.Dir(p)),
				zap.Error(err),
			)
			return nil, sftp.ErrSshFxFailure
		}

		file, err := os.Create(p)
		if err != nil {
			logger.Get().Errorw("error creating file", zap.String("source", p), zap.Error(err))
			return nil, sftp.ErrSshFxFailure
		}

		return file, nil
	} else if err != nil {
		logger.Get().Errorw("error performing file stat", zap.String("source", p), zap.Error(err))
		return nil, sftp.ErrSshFxFailure
	}

	// If we've made it here it means the file already exists and we don't need to do anything
	// fancy to handle it. Just pass over the request flags so the system knows what the end
	// goal with the file is going to be.
	//
	// But first, check that the user has permission to save modified files.
	if !fs.can("save-files") {
		return nil, sftp.ErrSshFxPermissionDenied
	}

	file, err := os.OpenFile(p, int(request.Flags), 0644)
	if err != nil {
		logger.Get().Errorw("error writing to existing file",
			zap.Uint32("flags", request.Flags),
			zap.String("source", p),
			zap.Error(err),
		)
		return nil, sftp.ErrSshFxFailure
	}

	return file, nil
}

// Hander for basic SFTP system calls related to files, but not anything to do with reading
// or writing to those files.
func (fs FileSystem) Filecmd(request *sftp.Request) error {
	if fs.readOnly {
		return sftp.ErrSshFxOpUnsupported
	}

	p, err := fs.buildPath(request.Filepath)
	if err != nil {
		return sftp.ErrSshFxNoSuchFile
	}

	var target string
	// If a target is provided in this request validate that it is going to the correct
	// location for the server. If it is not, return an operation unsupported error. This
	// is maybe not the best error response, but its not wrong either.
	if request.Target != "" {
		target, err = fs.buildPath(request.Target)
		if err != nil {
			return sftp.ErrSshFxOpUnsupported
		}
	}

	switch request.Method {
	// Need to add this in eventually, should work similarly to the current daemon.
	case "SetStat", "Setstat":
		return sftp.ErrSshFxOpUnsupported
	case "Rename":
		if !fs.can("move-files") {
			return sftp.ErrSshFxPermissionDenied
		}

		if err := os.Rename(p, target); err != nil {
			logger.Get().Errorw("failed to rename file",
				zap.String("source", p),
				zap.String("target", target),
				zap.Error(err),
			)
			return sftp.ErrSshFxFailure
		}

		return sftp.ErrSshFxOk
	case "Rmdir":
		if !fs.can("delete-files") {
			return sftp.ErrSshFxPermissionDenied
		}

		if err := os.RemoveAll(p); err != nil {
			logger.Get().Errorw("failed to remove directory", zap.String("source", p), zap.Error(err))
			return sftp.ErrSshFxFailure
		}

		return sftp.ErrSshFxOk
	case "Mkdir":
		if !fs.can("create-files") {
			return sftp.ErrSshFxPermissionDenied
		}

		if err := os.MkdirAll(p, 0755); err != nil {
			logger.Get().Errorw("failed to create directory", zap.String("source", p), zap.Error(err))
			return sftp.ErrSshFxFailure
		}

		return sftp.ErrSshFxOk
	case "Symlink":
		if !fs.can("create-files") {
			return sftp.ErrSshFxPermissionDenied
		}

		if err := os.Symlink(p, target); err != nil {
			logger.Get().Errorw("failed to create symlink",
				zap.String("source", p),
				zap.String("target", target),
				zap.Error(err),
			)
			return sftp.ErrSshFxFailure
		}

		return sftp.ErrSshFxOk
	case "Remove":
		if !fs.can("delete-files") {
			return sftp.ErrSshFxPermissionDenied
		}

		if err := os.Remove(p); err != nil {
			logger.Get().Errorw("failed to remove a file", zap.String("source", p), zap.Error(err))
			return sftp.ErrSshFxFailure
		}

		return sftp.ErrSshFxOk
	default:
		return sftp.ErrSshFxOpUnsupported
	}
}

// Handler for SFTP filesystem list calls. This will handle calls to list the contents of
// a directory as well as perform file/folder stat calls.
func (fs FileSystem) Filelist(request *sftp.Request) (sftp.ListerAt, error) {
	p, err := fs.buildPath(request.Filepath)
	if err != nil {
		return nil, sftp.ErrSshFxNoSuchFile
	}

	switch request.Method {
	case "List":
		if !fs.can("list-files") {
			return nil, sftp.ErrSshFxPermissionDenied
		}

		files, err := ioutil.ReadDir(p)
		if err != nil {
			logger.Get().Error("error listing directory", zap.Error(err))
			return nil, sftp.ErrSshFxFailure
		}

		return ListerAt(files), nil
	case "Stat":
		if !fs.can("list-files") {
			return nil, sftp.ErrSshFxPermissionDenied
		}

		file, err := os.Open(p)
		defer file.Close()

		if err != nil {
			logger.Get().Error("error opening file for stat", zap.Error(err))
			return nil, sftp.ErrSshFxFailure
		}

		s, err := file.Stat()
		if err != nil {
			logger.Get().Error("error statting file", zap.Error(err))
			return nil, sftp.ErrSshFxFailure
		}

		return ListerAt([]os.FileInfo{s}), nil
	default:
		// Before adding readlink support we need to evaluate any potential security risks
		// as a result of navigating around to a location that is outside the home directory
		// for the logged in user. I don't forsee it being much of a problem, but I do want to
		// check it out before slapping some code here. Until then, we'll just return an
		// unsupported response code.
		return nil, sftp.ErrSshFxOpUnsupported
	}
}

// Normalizes a directory we get from the SFTP request to ensure the user is not able to escape
// from their data directory. After normalization if the directory is still within their home
// path it is returned. If they managed to "escape" an error will be returned.
func (fs FileSystem) buildPath(rawPath string) (string, error) {
	// Calling filepath.Clean on the joined directory will resolve it to the absolute path,
	// removing any ../ type of path resolution, and leaving us with the absolute final path.
	p := filepath.Clean(filepath.Join(fs.directory, rawPath))

	// If the new path doesn't start with their root directory there is clearly an escape
	// attempt going on, and we should NOT resolve this path for them.
	if !strings.HasPrefix(p, fs.directory) {
		return "", errors.New("invalid path resolution")
	}

	return p, nil
}

// Determines if a user has permission to perform a specific action on the SFTP server. These
// permissions are defined and returned by the Panel API.
func (fs FileSystem) can(permission string) bool {
	// Server owners and super admins have their permissions returned as '[*]' via the Panel
	// API, so for the sake of speed do an initial check for that before iterating over the
	// entire array of permissions.
	if len(fs.permissions) == 1 && fs.permissions[0] == "*" {
		return true
	}

	// Not the owner or an admin, loop over the permissions that were returned to determine
	// if they have the passed permission.
	for _, p := range fs.permissions {
		if p == permission {
			return true
		}
	}

	return false
}
