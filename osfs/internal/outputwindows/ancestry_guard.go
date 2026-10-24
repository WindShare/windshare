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

	"github.com/windshare/windshare/osfs/internal/outputfault"
	"golang.org/x/sys/windows"
)

const windowsAncestryGuardAccess = windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES |
	windows.SYNCHRONIZE

type windowsAncestryGuardScope uint8

const (
	windowsGuardPublicOutputRoot windowsAncestryGuardScope = iota + 1
	windowsGuardExternalPlacement
	windowsGuardPrivateRootCreation
	windowsGuardPrivatePublicationRoot
)

type windowsAncestryDirectoryOpener interface {
	Open(
		root windows.Handle,
		path string,
		access uint32,
		objectAttributes uint32,
	) (windows.Handle, uintptr, error)
}

type nativeWindowsAncestryDirectoryOpener struct{}

func (nativeWindowsAncestryDirectoryOpener) Open(
	root windows.Handle,
	path string,
	access uint32,
	objectAttributes uint32,
) (windows.Handle, uintptr, error) {
	return windowsOpenNativeWithOptions(
		root,
		path,
		access,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE,
		0,
		nil,
		windowsDirectoryShareMode(true),
		objectAttributes,
	)
}

type windowsPublicOperationGuard struct {
	mu   sync.Mutex
	root *windowsDirectory
	pins []*os.File
}

func (guard *windowsPublicOperationGuard) Root() *windowsDirectory {
	if guard == nil {
		return nil
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	return guard.root
}

func (guard *windowsPublicOperationGuard) Close() error {
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

func (platform *nativeOutputPlatform) acquirePublicOperationGuard() (
	_ *windowsPublicOperationGuard,
	resultErr error,
) {
	return platform.acquireDirectoryAncestryGuard(windowsGuardPublicOutputRoot)
}

func (platform *nativeOutputPlatform) acquireExternalPlacementGuard() (
	_ *windowsPublicOperationGuard,
	resultErr error,
) {
	return platform.acquireDirectoryAncestryGuard(windowsGuardExternalPlacement)
}

func (platform *nativeOutputPlatform) acquirePrivateRootCreationGuard() (
	_ *windowsPublicOperationGuard,
	resultErr error,
) {
	return platform.acquireDirectoryAncestryGuard(windowsGuardPrivateRootCreation)
}

func (platform *nativeOutputPlatform) acquirePrivatePublicationRootGuard() (
	_ *windowsPublicOperationGuard,
	resultErr error,
) {
	return platform.acquireDirectoryAncestryGuard(windowsGuardPrivatePublicationRoot)
}

func (platform *nativeOutputPlatform) acquireDirectoryAncestryGuard(
	scope windowsAncestryGuardScope,
) (_ *windowsPublicOperationGuard, resultErr error) {
	return platform.acquireDirectoryAncestryGuardWithOpener(
		scope, nativeWindowsAncestryDirectoryOpener{},
	)
}

func (platform *nativeOutputPlatform) acquireDirectoryAncestryGuardWithOpener(
	scope windowsAncestryGuardScope,
	opener windowsAncestryDirectoryOpener,
) (_ *windowsPublicOperationGuard, resultErr error) {
	operation, err := windowsAncestryGuardOperation(scope)
	if err != nil {
		return nil, windowsFailure(operation, "", errWindowsOutputUnsafe, err)
	}
	if platform == nil || platform.root == nil {
		return nil, windowsFailure(operation, "", errWindowsOutputUnsafe,
			errors.New("windows output platform is closed"))
	}
	if opener == nil {
		return nil, windowsFailure(operation, "", errWindowsOutputUnsafe,
			errors.New("windows ancestry directory opener is absent"))
	}
	root := platform.root
	if err := root.usable(); err != nil {
		return nil, err
	}
	paths, err := windowsAbsoluteDirectoryAncestry(root.path)
	if err != nil {
		return nil, windowsFailure(operation, root.path, errWindowsOutputUnsupported, err)
	}
	guard := &windowsPublicOperationGuard{pins: make([]*os.File, 0, len(paths)-1)}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, guard.Close())
		}
	}()

	traversal := windowsAncestryGuardTraversal{
		operation:  operation,
		scope:      scope,
		root:       root,
		opener:     opener,
		paths:      paths,
		rootAccess: windowsAncestryRootAccess(scope),
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
		guard.root = &windowsDirectory{
			file: file, metadataHandle: windows.InvalidHandle,
			path: root.path, volume: root.volume,
			objectIDs: root.objectIDs, objectIDState: newWindowsPersistentObjectIDState(),
			inspector: root.inspector, policy: root.policy, ancestryAuthority: root.ancestryAuthority,
			enumerate: root.enumerate, createObserver: root.createObserver,
			private:        scope == windowsGuardPrivatePublicationRoot,
			placementGuard: true, selfPlacementGuard: true,
		}
	}

	if !guard.root.private {
		guard.root.metadataHandle, guard.root.metadataOpenErr = root.openDirectoryMetadataHandle(".", guard.root)
		guard.root.metadataHandleOpen = guard.root.metadataOpenErr == nil
	}
	same, err := sameWindowsOpenedDirectory(root, guard.root)
	if err != nil || !same {
		return nil, windowsFailure(operation, root.path, errWindowsOutputUnsafe,
			errors.Join(errors.New("guarded root differs from the primary output-root authority"), err))
	}
	return guard, nil
}

func windowsAncestryGuardOperation(scope windowsAncestryGuardScope) (string, error) {
	switch scope {
	case windowsGuardPublicOutputRoot:
		return "acquire public output ancestry guard", nil
	case windowsGuardExternalPlacement:
		return "acquire external output placement guard", nil
	case windowsGuardPrivateRootCreation:
		return "acquire private publication-root creation guard", nil
	case windowsGuardPrivatePublicationRoot:
		return "acquire private publication-root guard", nil
	default:
		return "acquire output ancestry guard", errors.New("windows ancestry guard scope is invalid")
	}
}

type windowsAncestryGuardTraversal struct {
	operation  string
	scope      windowsAncestryGuardScope
	root       *windowsDirectory
	opener     windowsAncestryDirectoryOpener
	paths      []string
	rootAccess uint32
}

func (traversal windowsAncestryGuardTraversal) openEntry(
	index int,
	parentHandle windows.Handle,
	parentCaseSensitive bool,
) (*os.File, windowsObjectFacts, error) {
	path := traversal.paths[index]
	rootEntry := index == len(traversal.paths)-1
	openRoot, openPath, access, objectAttributes := windowsAncestryOpenParameters(
		path, index, rootEntry, parentHandle, parentCaseSensitive, traversal.rootAccess,
	)
	handle, _, err := traversal.opener.Open(openRoot, openPath, access, objectAttributes)
	if err != nil {
		category := errWindowsOutputUnsupported
		cause := err
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			category = errWindowsOutputUnsafe
			cause = errors.Join(outputfault.ErrAncestryAuthorityDenied, err)
		}
		return nil, windowsObjectFacts{}, windowsFailure(
			traversal.operation, path, category, cause,
		)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, windowsObjectFacts{}, windowsFailure(
			traversal.operation, path, errWindowsOutputUnsafe,
			errors.New("wrap guarded ancestry handle"),
		)
	}
	facts, inspectErr := traversal.root.inspector.Inspect(handle)
	validateErr := traversal.validateEntry(handle, openPath, index, rootEntry, parentCaseSensitive, facts, inspectErr)
	if cause := errors.Join(inspectErr, validateErr); cause != nil {
		class := errWindowsOutputUnsafe
		if errors.Is(cause, errWindowsOutputUnsupported) {
			class = errWindowsOutputUnsupported
		}
		return nil, windowsObjectFacts{}, errors.Join(
			windowsFailure(traversal.operation, path, class, cause), file.Close(),
		)
	}
	return file, facts, nil
}

func windowsAncestryOpenParameters(
	path string,
	index int,
	rootEntry bool,
	parentHandle windows.Handle,
	parentCaseSensitive bool,
	rootAccess uint32,
) (windows.Handle, string, uint32, uint32) {
	access := uint32(windowsAncestryGuardAccess)
	if rootEntry {
		access = rootAccess
	}
	openRoot := windows.Handle(0)
	openPath := windowsNTPath(path)
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

func windowsAncestryRootAccess(scope windowsAncestryGuardScope) uint32 {
	switch scope {
	case windowsGuardPrivateRootCreation:
		return windowsPrivateRootParentAccess()
	case windowsGuardPrivatePublicationRoot:
		return windowsPrivatePublicationRootAccess()
	default:
		return windowsRootDirectoryAccess()
	}
}

func (traversal windowsAncestryGuardTraversal) validateEntry(
	handle windows.Handle,
	openPath string,
	index int,
	rootEntry bool,
	parentCaseSensitive bool,
	facts windowsObjectFacts,
	inspectErr error,
) error {
	if inspectErr != nil {
		return nil
	}
	var err error
	if rootEntry && (traversal.scope == windowsGuardPublicOutputRoot ||
		traversal.scope == windowsGuardPrivatePublicationRoot) {
		err = validateWindowsRootShape(facts)
		if err == nil {
			err = windowsValidateOpenedObject(facts, traversal.root.volume, true)
		}
		if err == nil && traversal.scope == windowsGuardPrivatePublicationRoot {
			err = traversal.root.policy.verify(handle, true)
		}
	} else {
		err = validateWindowsExternalPlacement(facts, traversal.root.volume)
	}
	if err != nil || index == 0 {
		return err
	}
	return windowsVerifyOpenedPlacementLeafAuthority(handle, openPath, parentCaseSensitive)
}

func windowsAbsoluteDirectoryAncestry(path string) ([]string, error) {
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
