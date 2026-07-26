//go:build windows

package osfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

const windowsV3AncestryGuardAccess = windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES |
	windows.READ_CONTROL | windows.SYNCHRONIZE

type windowsV3AncestryGuardScope uint8

const (
	windowsV3GuardPublicOutputRoot windowsV3AncestryGuardScope = iota + 1
	windowsV3GuardExternalPlacement
)

type windowsV3AncestryDirectoryOpener interface {
	Open(
		root windows.Handle,
		path string,
		access uint32,
		objectAttributes uint32,
	) (windows.Handle, uintptr, error)
}

type nativeWindowsV3AncestryDirectoryOpener struct{}

func (nativeWindowsV3AncestryDirectoryOpener) Open(
	root windows.Handle,
	path string,
	access uint32,
	objectAttributes uint32,
) (windows.Handle, uintptr, error) {
	return windowsV3OpenNativeWithOptions(
		root,
		path,
		access,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE,
		0,
		nil,
		windowsV3DirectoryShareMode(true),
		objectAttributes,
	)
}

type windowsV3PublicOperationGuard struct {
	mu   sync.Mutex
	root *windowsV3Directory
	pins []*os.File
}

func (guard *windowsV3PublicOperationGuard) Root() *windowsV3Directory {
	if guard == nil {
		return nil
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	return guard.root
}

func (guard *windowsV3PublicOperationGuard) Close() error {
	if guard == nil {
		return nil
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	var result error
	if guard.root != nil {
		result = errors.Join(result, guard.root.Close())
		guard.root = nil
	}
	for index := len(guard.pins) - 1; index >= 0; index-- {
		result = errors.Join(result, guard.pins[index].Close())
	}
	guard.pins = nil
	return result
}

func (platform *windowsV3OutputPlatform) acquirePublicOperationGuard() (
	_ *windowsV3PublicOperationGuard,
	resultErr error,
) {
	return platform.acquireDirectoryAncestryGuard(windowsV3GuardPublicOutputRoot)
}

func (platform *windowsV3OutputPlatform) acquireExternalPlacementGuard() (
	_ *windowsV3PublicOperationGuard,
	resultErr error,
) {
	return platform.acquireDirectoryAncestryGuard(windowsV3GuardExternalPlacement)
}

func (platform *windowsV3OutputPlatform) acquireDirectoryAncestryGuard(
	scope windowsV3AncestryGuardScope,
) (_ *windowsV3PublicOperationGuard, resultErr error) {
	return platform.acquireDirectoryAncestryGuardWithOpener(
		scope, nativeWindowsV3AncestryDirectoryOpener{},
	)
}

func (platform *windowsV3OutputPlatform) acquireDirectoryAncestryGuardWithOpener(
	scope windowsV3AncestryGuardScope,
	opener windowsV3AncestryDirectoryOpener,
) (_ *windowsV3PublicOperationGuard, resultErr error) {
	operation := "acquire public output ancestry guard"
	switch scope {
	case windowsV3GuardPublicOutputRoot:
	case windowsV3GuardExternalPlacement:
		operation = "acquire external output placement guard"
	default:
		return nil, windowsV3Failure(operation, "", errWindowsV3OutputUnsafe,
			errors.New("Windows ancestry guard scope is invalid"))
	}
	if platform == nil || platform.root == nil {
		return nil, windowsV3Failure(operation, "", errWindowsV3OutputUnsafe,
			errors.New("Windows output platform is closed"))
	}
	if opener == nil {
		return nil, windowsV3Failure(operation, "", errWindowsV3OutputUnsafe,
			errors.New("Windows ancestry directory opener is absent"))
	}
	root := platform.root
	if err := root.usable(); err != nil {
		return nil, err
	}
	paths, err := windowsV3AbsoluteDirectoryAncestry(root.path)
	if err != nil {
		return nil, windowsV3Failure(operation, root.path, errWindowsV3OutputUnsupported, err)
	}
	guard := &windowsV3PublicOperationGuard{pins: make([]*os.File, 0, len(paths)-1)}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, guard.Close())
		}
	}()

	parentHandle := windows.Handle(0)
	parentCaseSensitive := false
	for index, path := range paths {
		rootEntry := index == len(paths)-1
		access := uint32(windowsV3AncestryGuardAccess)
		if rootEntry {
			access = windowsV3RootDirectoryAccess()
		}
		openRoot := windows.Handle(0)
		openPath := windowsV3NTPath(path)
		objectAttributes := uint32(windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE)
		if index > 0 {
			// Only the drive root is resolved absolutely. Every descendant is opened
			// beneath the preceding no-delete-share handle so a later string lookup
			// can never silently replace an already certified prefix.
			openRoot = parentHandle
			openPath = filepath.Base(path)
			if parentCaseSensitive {
				objectAttributes = windows.OBJ_DONT_REPARSE
			}
		}
		handle, _, err := opener.Open(openRoot, openPath, access, objectAttributes)
		if err != nil {
			return nil, windowsV3Failure(operation, path, errWindowsV3OutputUnsupported, err)
		}
		file := os.NewFile(uintptr(handle), path)
		if file == nil {
			_ = windows.CloseHandle(handle)
			return nil, windowsV3Failure(operation, path, errWindowsV3OutputUnsafe,
				errors.New("wrap guarded ancestry handle"))
		}

		facts, inspectErr := root.inspector.Inspect(handle)
		validateErr := error(nil)
		if inspectErr == nil {
			if rootEntry && scope == windowsV3GuardPublicOutputRoot {
				validateErr = validateWindowsV3Certification(facts)
				if validateErr == nil {
					validateErr = windowsV3ValidateOpenedObject(facts, root.volume, true)
				}
			} else {
				validateErr = validateWindowsV3ExternalPlacement(facts, root.volume)
			}
			if validateErr == nil && index > 0 {
				validateErr = windowsV3VerifyOpenedLeafAuthority(handle, openPath, parentCaseSensitive)
			}
		}
		authorityErr := error(nil)
		// Windows pins every external component against delete sharing, so ambient
		// ACLs above the output root cannot move that placement during an operation.
		// The persistent root Object ID detects any replacement in the unguarded
		// gap. Cross-principal ACL admission therefore starts at the output root;
		// selected descendants repeat it when their claims are prepared.
		if rootEntry && scope == windowsV3GuardPublicOutputRoot && inspectErr == nil && validateErr == nil {
			if root.ancestryAuthority == nil {
				authorityErr = errors.New("Windows ancestry authority verifier is absent")
			} else {
				authorityErr = root.ancestryAuthority.Verify(handle)
				if authorityErr != nil {
					authorityErr = errors.Join(errOutputAncestryAuthorityDenied, authorityErr)
				}
			}
		}
		if inspectErr != nil || validateErr != nil || authorityErr != nil {
			class := errWindowsV3OutputUnsafe
			if errors.Is(validateErr, errWindowsV3OutputUnsupported) ||
				errors.Is(authorityErr, errWindowsV3OutputUnsupported) {
				class = errWindowsV3OutputUnsupported
			}
			return nil, errors.Join(
				windowsV3Failure(operation, path, class, errors.Join(inspectErr, validateErr, authorityErr)),
				file.Close(),
			)
		}

		if !rootEntry {
			guard.pins = append(guard.pins, file)
			parentHandle = handle
			parentCaseSensitive = facts.caseSensitive
			continue
		}
		guard.root = &windowsV3Directory{
			file: file, path: root.path, volume: root.volume,
			objectIDs: root.objectIDs, objectIDState: newWindowsV3PersistentObjectIDState(),
			inspector: root.inspector, policy: root.policy, ancestryAuthority: root.ancestryAuthority,
			enumerate: root.enumerate, createObserver: root.createObserver,
			placementGuard: true, selfPlacementGuard: true,
		}
	}

	same, err := sameWindowsV3OpenedDirectory(root, guard.root)
	if err != nil || !same {
		return nil, windowsV3Failure(operation, root.path, errWindowsV3OutputUnsafe,
			errors.Join(errors.New("guarded root differs from the primary output-root authority"), err))
	}
	return guard, nil
}

func windowsV3AbsoluteDirectoryAncestry(path string) ([]string, error) {
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, `\\?\UNC\`) || strings.HasPrefix(clean, `\\`) &&
		!strings.HasPrefix(clean, `\\?\`) {
		return nil, errors.New("UNC ancestry is not certified")
	}
	clean = strings.TrimPrefix(clean, `\\?\`)
	if !filepath.IsAbs(clean) {
		return nil, errors.New("output-root ancestry must be absolute")
	}
	volume := filepath.VolumeName(clean)
	if len(volume) != 2 || volume[1] != ':' {
		return nil, fmt.Errorf("output-root ancestry has unsupported local volume %q", volume)
	}
	volumeRoot := volume + string(filepath.Separator)
	relative, err := filepath.Rel(volumeRoot, clean)
	if err != nil || relative == ".." || strings.HasPrefix(relative, `..\`) {
		return nil, errors.Join(errors.New("output root escapes its local volume"), err)
	}
	paths := []string{volumeRoot}
	if relative == "." {
		return paths, nil
	}
	current := volumeRoot
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return nil, errors.New("output-root ancestry contains a non-canonical component")
		}
		current = filepath.Join(current, component)
		paths = append(paths, current)
	}
	return paths, nil
}
