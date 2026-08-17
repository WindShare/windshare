//go:build windows

package outputwindows

import (
	"bytes"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

type windowsOutputV3Platform struct {
	native              *windowsV3OutputPlatform
	root                *windowsOutputV3Directory
	rootOpenDisposition outputcap.RootOpenDisposition
	publicationGuard    *windowsV3PublicOperationGuard
}

type windowsOutputV3Directory struct {
	native *windowsV3Directory
}

type windowsOutputV3PublicOperationGuard struct {
	native *windowsV3PublicOperationGuard
	root   *windowsOutputV3Directory
}

type windowsOutputV3EntryRef struct {
	native *windowsV3PinnedEntry
}

type windowsOutputV3FileState struct {
	native   *windowsV3File
	private  bool
	borrowed bool
}

type windowsOutputV3ObservedFile struct {
	state *windowsOutputV3FileState
}

type windowsOutputV3RecoveryDurabilityFile struct {
	state *windowsOutputV3FileState
}

type windowsOutputV3MutableFile struct {
	state *windowsOutputV3FileState
}

type windowsOutputV3Lock struct {
	native *windowsV3StableLock
	file   *windowsOutputV3MutableFile
}

func Open(path string, create bool) (outputcap.Platform, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace,
			windowsV3Failure("open output root", path, errWindowsV3OutputUnsafe,
				errors.New("output root must be absolute")))
	}
	clean := filepath.Clean(path)
	rootOpenDisposition := outputcap.CallerProvidedContainer
	native, err := openWindowsV3OutputPlatform(clean)
	if create && errors.Is(err, fs.ErrNotExist) {
		native, err = windowsCreateCertifiedOutputRoot(clean)
		if err == nil {
			rootOpenDisposition = outputcap.AuthorityCreatedRoot
		}
	}
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Platform{
		native:              native,
		root:                &windowsOutputV3Directory{native: native.root},
		rootOpenDisposition: rootOpenDisposition,
	}, nil
}

// OpenPrivatePublicationRoot returns a root whose complete ancestry remains
// pinned for the caller's transaction. Missing roots are created as protected
// private directories; existing roots are only reopened and certified.
func OpenPrivatePublicationRoot(path string, create bool) (outputcap.Platform, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace,
			windowsV3Failure("open private publication root", path, errWindowsV3OutputUnsafe,
				errors.New("private publication root must be absolute")))
	}
	clean := filepath.Clean(path)
	native, err := openWindowsV3OutputPlatform(clean)
	if err == nil {
		platform, retainErr := retainWindowsV3PrivatePublicationRoot(native)
		if retainErr != nil {
			return nil, windowsOutputV3Error(retainErr)
		}
		return platform, nil
	}
	if !create || !errors.Is(err, fs.ErrNotExist) {
		return nil, windowsOutputV3Error(err)
	}
	platform, err := createWindowsV3PrivatePublicationRootWithObserver(clean, nil)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return platform, nil
}

func retainWindowsV3PrivatePublicationRoot(
	native *windowsV3OutputPlatform,
) (*windowsOutputV3Platform, error) {
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
	return &windowsOutputV3Platform{
		native:              native,
		root:                &windowsOutputV3Directory{native: root},
		rootOpenDisposition: outputcap.CallerProvidedContainer,
		publicationGuard:    guard,
	}, nil
}

func createWindowsV3PrivatePublicationRootWithObserver(
	path string,
	observer windowsV3OutputRootCreateObserver,
) (result *windowsOutputV3Platform, resultErr error) {
	const operation = "create private publication root"
	parentPath := filepath.Dir(path)
	name := filepath.Base(path)
	if parentPath == path || name == "." || name == string(filepath.Separator) {
		return nil, windowsV3Failure(operation, path, errWindowsV3OutputUnsafe,
			errors.New("private publication root must be one missing child of an existing parent"))
	}
	parent, err := openWindowsV3PrivateRootParent(parentPath)
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
			windowsV3Failure(operation, parentPath, errWindowsV3OutputUnsafe,
				errors.New("private publication-root creation guard has no parent authority")),
			placement.Close(),
			parent.Close(),
		)
	}
	if err := windowsV3ObserveOutputRootCreate(
		observer,
		parentPath,
		windowsV3OutputRootCreatePlacementPinned,
	); err != nil {
		return nil, errors.Join(err, placement.Close(), parent.Close())
	}
	created, err := parentRoot.CreatePrivateDirectory(name)
	if err != nil {
		return nil, errors.Join(err, placement.Close(), parent.Close())
	}
	var createdClaim []byte
	var pending *windowsV3OutputPlatform
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
			resultErr = errors.Join(resultErr, cleanupWindowsV3PrivatePublicationRoot(
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

	pending, err = openWindowsV3OutputPlatform(path)
	if err != nil {
		return nil, err
	}
	same, compareErr := sameWindowsV3OpenedDirectory(created, pending.root)
	if compareErr != nil || !same {
		return nil, windowsV3Failure(operation, path, errWindowsV3OutputUnsafe,
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
	result, err = retainWindowsV3PrivatePublicationRoot(pending)
	pending = nil
	if err != nil {
		return nil, err
	}
	reopenedClaim, claimErr := result.root.native.preparePrivateIdentityClaim()
	if claimErr != nil || !bytes.Equal(reopenedClaim, createdClaim) {
		return result, windowsV3Failure(operation, path, errWindowsV3OutputUnsafe,
			errors.Join(errors.New("certified private publication root lost its persistent identity"), claimErr))
	}
	if err := windowsV3ObserveOutputRootCreate(
		observer,
		path,
		windowsV3OutputRootCreateComponentPinned,
	); err != nil {
		return result, err
	}
	result.rootOpenDisposition = outputcap.AuthorityCreatedRoot
	succeeded = true
	return result, nil
}

func cleanupWindowsV3PrivatePublicationRoot(
	parent *windowsV3Directory,
	created *windowsV3Directory,
	name string,
	path string,
	createdClaim []byte,
) (resultErr error) {
	const operation = "cleanup private publication root"
	if parent == nil {
		return windowsV3Failure(operation, path, errWindowsV3OutputUnsafe,
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
			return windowsV3Failure(operation, path, errWindowsV3OutputUnsafe,
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

type windowsV3OutputRootCreateCut uint8

const (
	windowsV3OutputRootCreatePlacementPinned windowsV3OutputRootCreateCut = iota + 1
	windowsV3OutputRootCreateComponentPinned
)

type windowsV3OutputRootCreateObserver interface {
	ObserveOutputRootCreate(path string, cut windowsV3OutputRootCreateCut) error
}

type windowsV3OutputRootCreateObserverFunc func(string, windowsV3OutputRootCreateCut) error

func (observe windowsV3OutputRootCreateObserverFunc) ObserveOutputRootCreate(
	path string,
	cut windowsV3OutputRootCreateCut,
) error {
	return observe(path, cut)
}

func windowsCreateCertifiedOutputRoot(path string) (*windowsV3OutputPlatform, error) {
	return windowsCreateCertifiedOutputRootWithObserver(path, nil)
}

func windowsCreateCertifiedOutputRootWithObserver(
	path string,
	observer windowsV3OutputRootCreateObserver,
) (result *windowsV3OutputPlatform, resultErr error) {
	const operation = "create certified Windows output root"
	ancestor, missing, err := windowsV3FindCertifiedOutputAncestor(path)
	if err != nil {
		return nil, err
	}
	placement, err := ancestor.acquireExternalPlacementGuard()
	if err != nil {
		return nil, errors.Join(err, ancestor.Close())
	}
	createdAuthorities := make([]*windowsV3Directory, 0, len(missing))
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
		keepResult, err := finishWindowsV3OutputRootCreate(resultErr, cleanupErr, closeResult)
		resultErr = err
		if !keepResult {
			result = nil
		}
	}()
	current := placement.Root()
	if current == nil {
		return nil, errors.New("external output placement guard has no root authority")
	}
	if err := windowsV3ObserveOutputRootCreate(observer, current.path, windowsV3OutputRootCreatePlacementPinned); err != nil {
		return nil, err
	}
	current, createdAuthorities, err = windowsV3CreateMissingOutputComponents(current, missing, observer)
	if err != nil {
		return nil, err
	}

	// The absolute no-reparse reopen binds the caller's requested spelling; the
	// same-object check prevents an attacker from substituting another directory
	// after the final handle-relative create.
	reopened, err := openWindowsV3OutputPlatform(path)
	if err != nil {
		return nil, err
	}
	same, compareErr := sameWindowsV3OpenedDirectory(current, reopened.root)
	if compareErr != nil || !same {
		return nil, errors.Join(windowsV3Failure(operation, path, errWindowsV3OutputUnsafe,
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

func windowsV3FindCertifiedOutputAncestor(path string) (*windowsV3OutputPlatform, []string, error) {
	const operation = "create certified Windows output root"
	candidate := path
	missing := make([]string, 0, 4)
	for {
		opened, err := openWindowsV3OutputPlatform(candidate)
		if err == nil {
			return opened, missing, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return nil, nil, errors.Join(windowsV3Failure(operation, path, errWindowsV3OutputUnsafe,
				errors.New("no existing certified ancestor contains the requested root")), err)
		}
		missing = append(missing, filepath.Base(candidate))
		candidate = parent
	}
}

func windowsV3CreateMissingOutputComponents(
	current *windowsV3Directory,
	missing []string,
	observer windowsV3OutputRootCreateObserver,
) (*windowsV3Directory, []*windowsV3Directory, error) {
	createdAuthorities := make([]*windowsV3Directory, 0, len(missing))
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
		if err := windowsV3ObserveOutputRootCreate(
			observer, current.path, windowsV3OutputRootCreateComponentPinned,
		); err != nil {
			return nil, createdAuthorities, err
		}
	}
	return current, createdAuthorities, nil
}

func windowsV3ObserveOutputRootCreate(
	observer windowsV3OutputRootCreateObserver,
	path string,
	cut windowsV3OutputRootCreateCut,
) error {
	if observer == nil {
		return nil
	}
	return observer.ObserveOutputRootCreate(path, cut)
}

func finishWindowsV3OutputRootCreate(
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
