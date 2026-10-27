//go:build windows

package outputwindows

import (
	"bytes"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputfile"
	"golang.org/x/sys/windows"
)

type windowsOutputPlatform struct {
	native              *nativeOutputPlatform
	root                *windowsOutputDirectory
	rootOpenDisposition outputcap.RootOpenDisposition
	publicationGuard    *windowsPublicOperationGuard
}

type windowsOutputDirectory struct {
	native *windowsDirectory
}

type windowsOutputPublicOperationGuard struct {
	native *windowsPublicOperationGuard
	root   *windowsOutputDirectory
}

type windowsOutputEntryRef struct {
	native *windowsPinnedEntry
}

type windowsOutputFileState struct {
	native   *windowsFile
	private  bool
	borrowed bool
}

type windowsOutputObservedFile = outputfile.Observed[*windowsOutputFileState]

type windowsOutputRecoveryDurabilityFile = outputfile.Recovery[*windowsOutputFileState]

type windowsOutputMutableFile = outputfile.Mutable[*windowsOutputFileState]

type windowsOutputLock struct {
	native *windowsStableLock
	file   *windowsOutputMutableFile
}

func Open(path string, create bool) (outputcap.Platform, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace,
			windowsFailure("open output root", path, errWindowsOutputUnsafe,
				errors.New("output root must be absolute")))
	}
	clean := filepath.Clean(path)
	rootOpenDisposition := outputcap.CallerProvidedContainer
	native, err := openWindowsOutputPlatform(clean)
	if create && errors.Is(err, fs.ErrNotExist) {
		native, err = windowsCreateCertifiedOutputRoot(clean)
		if err == nil {
			rootOpenDisposition = outputcap.AuthorityCreatedRoot
		}
	}
	if err != nil {
		return nil, windowsOutputError(err)
	}
	return &windowsOutputPlatform{
		native:              native,
		root:                &windowsOutputDirectory{native: native.root},
		rootOpenDisposition: rootOpenDisposition,
	}, nil
}

// OpenPrivatePublicationRoot returns a root whose complete ancestry remains
// pinned for the caller's transaction. Missing roots are created as protected
// private directories; existing roots are only reopened and certified.
func OpenPrivatePublicationRoot(path string, create bool) (outputcap.Platform, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace,
			windowsFailure("open private publication root", path, errWindowsOutputUnsafe,
				errors.New("private publication root must be absolute")))
	}
	clean := filepath.Clean(path)
	native, err := openWindowsOutputPlatform(clean)
	if err == nil {
		platform, retainErr := retainWindowsPrivatePublicationRoot(native)
		if retainErr != nil {
			return nil, windowsOutputError(retainErr)
		}
		return platform, nil
	}
	if !create || !errors.Is(err, fs.ErrNotExist) {
		return nil, windowsOutputError(err)
	}
	platform, err := createWindowsPrivatePublicationRootWithObserver(clean, nil)
	if err != nil {
		return nil, windowsOutputError(err)
	}
	return platform, nil
}

func retainWindowsPrivatePublicationRoot(
	native *nativeOutputPlatform,
) (*windowsOutputPlatform, error) {
	if native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("private publication root has no native platform"))
	}
	guard, err := native.acquirePrivatePublicationRootGuard()
	if err != nil {
		return nil, errors.Join(err, native.Close())
	}
	root := guard.Root()
	if root == nil {
		return nil, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("private publication root guard has no root authority"),
			guard.Close(),
			native.Close(),
		)
	}
	return &windowsOutputPlatform{
		native:              native,
		root:                &windowsOutputDirectory{native: root},
		rootOpenDisposition: outputcap.CallerProvidedContainer,
		publicationGuard:    guard,
	}, nil
}

func createWindowsPrivatePublicationRootWithObserver(
	path string,
	observer windowsOutputRootCreateObserver,
) (result *windowsOutputPlatform, resultErr error) {
	const operation = "create private publication root"
	parentPath := filepath.Dir(path)
	name := filepath.Base(path)
	if parentPath == path || name == "." || name == string(filepath.Separator) {
		return nil, windowsFailure(operation, path, errWindowsOutputUnsafe,
			errors.New("private publication root must be one missing child of an existing parent"))
	}
	parent, err := openWindowsPrivateRootParent(parentPath)
	if err != nil {
		return nil, err
	}
	placement, err := parent.acquirePrivateRootCreationGuard()
	if err != nil {
		return nil, errors.Join(err, parent.Close())
	}
	parentRoot := placement.Root()
	if parentRoot == nil {
		return nil, errors.Join(
			windowsFailure(operation, parentPath, errWindowsOutputUnsafe,
				errors.New("private publication-root creation guard has no parent authority")),
			placement.Close(),
			parent.Close(),
		)
	}
	if err := windowsObserveOutputRootCreate(
		observer,
		parentPath,
		windowsOutputRootCreatePlacementPinned,
	); err != nil {
		return nil, errors.Join(err, placement.Close(), parent.Close())
	}
	created, err := parentRoot.CreatePrivateDirectory(name)
	if err != nil {
		return nil, errors.Join(err, placement.Close(), parent.Close())
	}
	var createdClaim []byte
	var pending *nativeOutputPlatform
	succeeded := false
	defer func() {
		if pending != nil {
			resultErr = errors.Join(resultErr, pending.Close())
		}
		if !succeeded && result != nil {
			resultErr = errors.Join(resultErr, result.Close())
			result = nil
		}
		if !succeeded {
			resultErr = errors.Join(resultErr, cleanupWindowsPrivatePublicationRoot(
				parentRoot, created, name, path, createdClaim,
			))
		}
		settlementErr := errors.Join(created.Close(), placement.Close(), parent.Close())
		if settlementErr != nil {
			resultErr = errors.Join(resultErr, settlementErr)
			if result != nil {
				resultErr = errors.Join(resultErr, result.Close())
				result = nil
			}
		}
	}()

	pending, err = openWindowsOutputPlatform(path)
	if err != nil {
		return nil, err
	}
	same, compareErr := sameWindowsOpenedDirectory(created, pending.root)
	if compareErr != nil || !same {
		return nil, windowsFailure(operation, path, errWindowsOutputUnsafe,
			errors.Join(errors.New("certified private publication root changed identity"), compareErr))
	}
	createdClaim, err = created.preparePrivateIdentityClaim()
	if err != nil {
		return nil, err
	}
	if err := created.Close(); err != nil {
		return nil, err
	}
	created = nil
	result, err = retainWindowsPrivatePublicationRoot(pending)
	pending = nil
	if err != nil {
		return nil, err
	}
	reopenedClaim, claimErr := result.root.native.preparePrivateIdentityClaim()
	if claimErr != nil || !bytes.Equal(reopenedClaim, createdClaim) {
		return result, windowsFailure(operation, path, errWindowsOutputUnsafe,
			errors.Join(errors.New("certified private publication root lost its persistent identity"), claimErr))
	}
	if err := windowsObserveOutputRootCreate(
		observer,
		path,
		windowsOutputRootCreateComponentPinned,
	); err != nil {
		return result, err
	}
	result.rootOpenDisposition = outputcap.AuthorityCreatedRoot
	succeeded = true
	return result, nil
}

func cleanupWindowsPrivatePublicationRoot(
	parent *windowsDirectory,
	created *windowsDirectory,
	name string,
	path string,
	createdClaim []byte,
) (resultErr error) {
	const operation = "cleanup private publication root"
	if parent == nil {
		return windowsFailure(operation, path, errWindowsOutputUnsafe,
			errors.New("private publication-root parent authority is absent"))
	}
	cleanupTarget := created
	if cleanupTarget == nil {
		reopened, err := parent.OpenPrivateDirectory(name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		cleanupTarget = reopened
		defer func() { resultErr = errors.Join(resultErr, cleanupTarget.Close()) }()

		claim, claimErr := cleanupTarget.preparePrivateIdentityClaim()
		if claimErr != nil || len(createdClaim) == 0 || !bytes.Equal(claim, createdClaim) {
			return windowsFailure(operation, path, errWindowsOutputUnsafe,
				errors.Join(errors.New("private publication-root cleanup identity changed"), claimErr))
		}
	}
	// Cleanup is authorized by the retained object identity, not by ambient
	// DELETE_CHILD on the parent. That keeps rollback exact even under a parent
	// whose inherited ACL grants unrelated principals broad mutation rights.
	return errors.Join(
		parent.RemoveDirectory(name, cleanupTarget),
		parent.Sync(),
	)
}

type windowsOutputRootCreateCut uint8

const (
	windowsOutputRootCreatePlacementPinned windowsOutputRootCreateCut = iota + 1
	windowsOutputRootCreateComponentPinned
)

type windowsOutputRootCreateObserver interface {
	ObserveOutputRootCreate(path string, cut windowsOutputRootCreateCut) error
}

type windowsOutputRootCreateObserverFunc func(string, windowsOutputRootCreateCut) error

func (observe windowsOutputRootCreateObserverFunc) ObserveOutputRootCreate(
	path string,
	cut windowsOutputRootCreateCut,
) error {
	return observe(path, cut)
}

func windowsCreateCertifiedOutputRoot(path string) (*nativeOutputPlatform, error) {
	return windowsCreateCertifiedOutputRootWithObserver(path, nil)
}

func windowsCreateCertifiedOutputRootWithObserver(
	path string,
	observer windowsOutputRootCreateObserver,
) (result *nativeOutputPlatform, resultErr error) {
	const operation = "create certified Windows output root"
	ancestor, missing, err := windowsFindCertifiedOutputAncestor(path)
	if err != nil {
		return nil, err
	}
	placement, err := ancestor.acquireExternalPlacementGuard()
	if err != nil {
		return nil, errors.Join(err, ancestor.Close())
	}
	createdAuthorities := make([]*windowsDirectory, 0, len(missing))
	defer func() {
		var cleanupErr error
		for index := range slices.Backward(createdAuthorities) {
			cleanupErr = errors.Join(cleanupErr, createdAuthorities[index].Close())
		}
		cleanupErr = errors.Join(cleanupErr, placement.Close(), ancestor.Close())
		var closeResult func() error
		if result != nil {
			closeResult = result.Close
		}
		keepResult, err := finishWindowsOutputRootCreate(resultErr, cleanupErr, closeResult)
		resultErr = err
		if !keepResult {
			result = nil
		}
	}()
	current := placement.Root()
	if current == nil {
		return nil, errors.New("external output placement guard has no root authority")
	}
	if err := windowsObserveOutputRootCreate(observer, current.path, windowsOutputRootCreatePlacementPinned); err != nil {
		return nil, err
	}
	current, createdAuthorities, err = windowsCreateMissingOutputComponents(current, missing, observer)
	if err != nil {
		return nil, err
	}

	// The absolute no-reparse reopen binds the caller's requested spelling; the
	// same-object check prevents an attacker from substituting another directory
	// after the final handle-relative create.
	reopened, err := openWindowsOutputPlatform(path)
	if err != nil {
		return nil, err
	}
	same, compareErr := sameWindowsOpenedDirectory(current, reopened.root)
	if compareErr != nil || !same {
		return nil, errors.Join(windowsFailure(operation, path, errWindowsOutputUnsafe,
			errors.New("reopened root differs from the handle-created directory")), compareErr, reopened.Close())
	}
	// Ancestors are placement-only authorities, but the newly created output root
	// must satisfy the receiver/privileged-principal DACL policy before the caller
	// can use it for state or content.
	outputGuard, err := reopened.acquirePublicOperationGuard()
	if err != nil {
		return nil, errors.Join(err, reopened.Close())
	}
	if err := outputGuard.Close(); err != nil {
		return nil, errors.Join(err, reopened.Close())
	}
	return reopened, nil
}

func windowsFindCertifiedOutputAncestor(path string) (*nativeOutputPlatform, []string, error) {
	const operation = "create certified Windows output root"
	candidate := path
	missing := make([]string, 0, 4)
	for {
		opened, err := openWindowsOutputPlatform(candidate)
		if err == nil {
			return opened, missing, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return nil, nil, errors.Join(windowsFailure(operation, path, errWindowsOutputUnsafe,
				errors.New("no existing certified ancestor contains the requested root")), err)
		}
		missing = append(missing, filepath.Base(candidate))
		candidate = parent
	}
}

func windowsCreateMissingOutputComponents(
	current *windowsDirectory,
	missing []string,
	observer windowsOutputRootCreateObserver,
) (*windowsDirectory, []*windowsDirectory, error) {
	createdAuthorities := make([]*windowsDirectory, 0, len(missing))
	for index := range slices.Backward(missing) {
		created, err := current.openDirectory(missing[index], false, windows.FILE_CREATE)
		if err != nil {
			return nil, createdAuthorities, err
		}
		if err := errors.Join(created.Sync(), current.Sync()); err != nil {
			return nil, createdAuthorities, errors.Join(err, created.Close())
		}
		createdAuthorities = append(createdAuthorities, created)
		current = created
		if err := windowsObserveOutputRootCreate(
			observer, current.path, windowsOutputRootCreateComponentPinned,
		); err != nil {
			return nil, createdAuthorities, err
		}
	}
	return current, createdAuthorities, nil
}

func windowsObserveOutputRootCreate(
	observer windowsOutputRootCreateObserver,
	path string,
	cut windowsOutputRootCreateCut,
) error {
	if observer == nil {
		return nil
	}
	return observer.ObserveOutputRootCreate(path, cut)
}

func finishWindowsOutputRootCreate(
	operationErr error,
	cleanupErr error,
	closeResult func() error,
) (bool, error) {
	resultErr := errors.Join(operationErr, cleanupErr)
	if resultErr == nil {
		return true, nil
	}
	// A failed constructor cannot transfer a live primary handle. Keep the
	// original operation/cleanup classification first and append any secondary
	// close failure only as additional ownership evidence.
	if closeResult != nil {
		resultErr = errors.Join(resultErr, closeResult())
	}
	return false, resultErr
}
