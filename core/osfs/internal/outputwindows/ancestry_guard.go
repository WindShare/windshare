//go:build windows

package outputwindows

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"golang.org/x/sys/windows"
)

const windowsV3AncestryGuardAccess = windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES |
	windows.READ_CONTROL | windows.SYNCHRONIZE

type windowsV3AncestryGuardScope uint8

const (
	windowsV3GuardPublicOutputRoot windowsV3AncestryGuardScope = iota + 1
	windowsV3GuardExternalPlacement
	windowsV3GuardPrivateRootCreation
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
	for index := range slices.Backward(guard.pins) {
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

func (platform *windowsV3OutputPlatform) acquirePrivateRootCreationGuard() (
	_ *windowsV3PublicOperationGuard,
	resultErr error,
) {
	return platform.acquireDirectoryAncestryGuard(windowsV3GuardPrivateRootCreation)
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
	operation, err := windowsV3AncestryGuardOperation(scope)
	if err != nil {
		return nil, windowsV3Failure(operation, "", errWindowsV3OutputUnsafe, err)
	}
	if platform == nil || platform.root == nil {
		return nil, windowsV3Failure(operation, "", errWindowsV3OutputUnsafe,
			errors.New("windows output platform is closed"))
	}
	if opener == nil {
		return nil, windowsV3Failure(operation, "", errWindowsV3OutputUnsafe,
			errors.New("windows ancestry directory opener is absent"))
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

	traversal := windowsV3AncestryGuardTraversal{
		operation:  operation,
		scope:      scope,
		root:       root,
		opener:     opener,
		paths:      paths,
		rootAccess: windowsV3AncestryRootAccess(scope),
	}
	parentHandle := windows.Handle(0)
	parentCaseSensitive := false
	for index := range paths {
		rootEntry := index == len(paths)-1
		file, facts, err := traversal.openEntry(index, parentHandle, parentCaseSensitive)
		if err != nil {
			return nil, err
		}
		if !rootEntry {
			guard.pins = append(guard.pins, file)
			parentHandle = windows.Handle(file.Fd())
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

func windowsV3AncestryGuardOperation(scope windowsV3AncestryGuardScope) (string, error) {
	switch scope {
	case windowsV3GuardPublicOutputRoot:
		return "acquire public output ancestry guard", nil
	case windowsV3GuardExternalPlacement:
		return "acquire external output placement guard", nil
	case windowsV3GuardPrivateRootCreation:
		return "acquire private publication-root creation guard", nil
	default:
		return "acquire output ancestry guard", errors.New("windows ancestry guard scope is invalid")
	}
}

type windowsV3AncestryGuardTraversal struct {
	operation  string
	scope      windowsV3AncestryGuardScope
	root       *windowsV3Directory
	opener     windowsV3AncestryDirectoryOpener
	paths      []string
	rootAccess uint32
}

func (traversal windowsV3AncestryGuardTraversal) openEntry(
	index int,
	parentHandle windows.Handle,
	parentCaseSensitive bool,
) (*os.File, windowsV3HandleFacts, error) {
	path := traversal.paths[index]
	rootEntry := index == len(traversal.paths)-1
	openRoot, openPath, access, objectAttributes := windowsV3AncestryOpenParameters(
		path, index, rootEntry, parentHandle, parentCaseSensitive, traversal.rootAccess,
	)
	handle, _, err := traversal.opener.Open(openRoot, openPath, access, objectAttributes)
	if err != nil {
		return nil, windowsV3HandleFacts{}, windowsV3Failure(
			traversal.operation, path, errWindowsV3OutputUnsupported, err,
		)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, windowsV3HandleFacts{}, windowsV3Failure(
			traversal.operation, path, errWindowsV3OutputUnsafe,
			errors.New("wrap guarded ancestry handle"),
		)
	}
	facts, inspectErr := traversal.root.inspector.Inspect(handle)
	validateErr := traversal.validateEntry(handle, openPath, index, rootEntry, parentCaseSensitive, facts, inspectErr)
	authorityErr := traversal.verifyRootAuthority(handle, rootEntry, inspectErr, validateErr)
	if cause := errors.Join(inspectErr, validateErr, authorityErr); cause != nil {
		class := errWindowsV3OutputUnsafe
		if errors.Is(cause, errWindowsV3OutputUnsupported) {
			class = errWindowsV3OutputUnsupported
		}
		return nil, windowsV3HandleFacts{}, errors.Join(
			windowsV3Failure(traversal.operation, path, class, cause), file.Close(),
		)
	}
	return file, facts, nil
}

func windowsV3AncestryOpenParameters(
	path string,
	index int,
	rootEntry bool,
	parentHandle windows.Handle,
	parentCaseSensitive bool,
	rootAccess uint32,
) (windows.Handle, string, uint32, uint32) {
	access := uint32(windowsV3AncestryGuardAccess)
	if rootEntry {
		access = rootAccess
	}
	openRoot := windows.Handle(0)
	openPath := windowsV3NTPath(path)
	objectAttributes := uint32(windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE)
	if index == 0 {
		return openRoot, openPath, access, objectAttributes
	}
	// Only the drive root is resolved absolutely. Every descendant is opened
	// beneath the preceding no-delete-share handle so a later string lookup can
	// never silently replace an already certified prefix.
	openRoot = parentHandle
	openPath = filepath.Base(path)
	if parentCaseSensitive {
		objectAttributes = windows.OBJ_DONT_REPARSE
	}
	return openRoot, openPath, access, objectAttributes
}

func windowsV3AncestryRootAccess(scope windowsV3AncestryGuardScope) uint32 {
	if scope == windowsV3GuardPrivateRootCreation {
		return windowsV3PrivateRootParentAccess()
	}
	return windowsV3RootDirectoryAccess()
}

func (traversal windowsV3AncestryGuardTraversal) validateEntry(
	handle windows.Handle,
	openPath string,
	index int,
	rootEntry bool,
	parentCaseSensitive bool,
	facts windowsV3HandleFacts,
	inspectErr error,
) error {
	if inspectErr != nil {
		return nil
	}
	var err error
	if rootEntry && traversal.scope == windowsV3GuardPublicOutputRoot {
		err = validateWindowsV3Certification(facts)
		if err == nil {
			err = windowsV3ValidateOpenedObject(facts, traversal.root.volume, true)
		}
	} else {
		err = validateWindowsV3ExternalPlacement(facts, traversal.root.volume)
	}
	if err != nil || index == 0 {
		return err
	}
	return windowsV3VerifyOpenedPlacementLeafAuthority(handle, openPath, parentCaseSensitive)
}

func (traversal windowsV3AncestryGuardTraversal) verifyRootAuthority(
	handle windows.Handle,
	rootEntry bool,
	inspectErr error,
	validateErr error,
) error {
	if !rootEntry || traversal.scope != windowsV3GuardPublicOutputRoot || inspectErr != nil || validateErr != nil {
		return nil
	}
	// Windows pins every external component against delete sharing, so ambient
	// ACLs above the output root cannot move that placement during an operation.
	// Cross-principal ACL admission therefore starts at the output root.
	if traversal.root.ancestryAuthority == nil {
		return errors.New("windows ancestry authority verifier is absent")
	}
	if err := traversal.root.ancestryAuthority.Verify(handle); err != nil {
		return errors.Join(outputfault.ErrAncestryAuthorityDenied, err)
	}
	return nil
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
	for component := range strings.SplitSeq(relative, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return nil, errors.New("output-root ancestry contains a non-canonical component")
		}
		current = filepath.Join(current, component)
		paths = append(paths, current)
	}
	return paths, nil
}
