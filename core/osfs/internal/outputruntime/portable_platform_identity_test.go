package outputruntime

import (
	"errors"
	"fmt"
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
	if direct, ok := source.(*portableRuntimeFile); ok && direct.filesystem == filesystem {
		path, err := direct.currentPath()
		return path, direct.info, err
	}
	provider, ok := source.(outputcap.CloseRevalidationIdentityProvider)
	if !ok {
		return "", nil, outputcap.ErrUnsafeNamespace
	}
	expected, err := provider.CloseRevalidationIdentity()
	if err != nil {
		return "", nil, err
	}
	var foundPath string
	var foundInfo os.FileInfo
	walkErr := filepath.WalkDir(filesystem.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		identity := outputcap.NewTransientFileIdentity(
			portableRuntimeIdentityDomain+":"+filesystem.root,
			fmt.Appendf(nil, "file:%d", filesystem.objectID(info)),
		)
		if identity.Equal(expected) {
			foundPath, foundInfo = path, info
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return "", nil, walkErr
	}
	if foundPath == "" {
		return "", nil, outputcap.ErrUnsafeNamespace
	}
	return foundPath, foundInfo, nil
}

func (filesystem *portableRuntimeFilesystem) findMatchingDirectory(
	source outputcap.Directory,
) (string, os.FileInfo, error) {
	if direct, ok := source.(*portableRuntimeDirectory); ok && direct.filesystem == filesystem {
		path, err := direct.currentPath()
		return path, direct.info, err
	}
	if source == nil {
		return "", nil, outputcap.ErrUnsafeNamespace
	}
	expected, err := source.IdentityClaim()
	if err != nil {
		return "", nil, err
	}
	var foundPath string
	var foundInfo os.FileInfo
	walkErr := filepath.WalkDir(filesystem.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		identity := outputcap.NewPersistentDirectoryIdentity(fmt.Appendf(
			nil,
			"directory:%s:%d",
			filesystem.root,
			filesystem.objectID(info),
		))
		if identity.Equal(expected) {
			foundPath, foundInfo = path, info
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return "", nil, walkErr
	}
	if foundPath == "" {
		return "", nil, outputcap.ErrUnsafeNamespace
	}
	return foundPath, foundInfo, nil
}
