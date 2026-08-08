package outputruntime

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func (filesystem *portableRuntimeFilesystem) findObjectPath(
	info os.FileInfo,
	preferred string,
) (string, error) {
	if current, err := os.Stat(preferred); err == nil && os.SameFile(current, info) {
		return preferred, nil
	}
	var found string
	walkErr := filepath.WalkDir(filesystem.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		current, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if os.SameFile(current, info) {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return "", walkErr
	}
	if found == "" {
		return "", fs.ErrNotExist
	}
	return found, nil
}

func (filesystem *portableRuntimeFilesystem) findMatchingFile(
	source outputcap.File,
) (string, os.FileInfo, error) {
	direct, ok := source.(*portableRuntimeFile)
	if !ok || direct.filesystem != filesystem {
		return "", nil, outputcap.ErrUnsafeNamespace
	}
	path, err := direct.currentPath()
	return path, direct.info, err
}

func (filesystem *portableRuntimeFilesystem) findMatchingDirectory(
	source outputcap.Directory,
) (string, os.FileInfo, error) {
	direct, ok := source.(*portableRuntimeDirectory)
	if !ok || direct.filesystem != filesystem {
		return "", nil, outputcap.ErrUnsafeNamespace
	}
	path, err := direct.currentPath()
	return path, direct.info, err
}
