//go:build windows

package outputwindows

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/windows"
)

type windowsOutputV3Platform struct {
	native *windowsV3OutputPlatform
	root   *windowsOutputV3Directory
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

type windowsOutputV3File struct {
	native   *windowsV3File
	private  bool
	borrowed bool
}

type windowsOutputV3Lock struct {
	native *windowsV3StableLock
	file   *windowsOutputV3File
}

func Open(path string, create bool) (outputcap.Platform, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace,
			windowsV3Failure("open output root", path, errWindowsV3OutputUnsafe,
				errors.New("output root must be absolute")))
	}
	clean := filepath.Clean(path)
	native, err := openWindowsV3OutputPlatform(clean)
	if create && errors.Is(err, fs.ErrNotExist) {
		native, err = windowsCreateCertifiedOutputRoot(clean)
	}
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Platform{
		native: native,
		root:   &windowsOutputV3Directory{native: native.root},
	}, nil
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
		for index := len(createdAuthorities) - 1; index >= 0; index-- {
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
	for index := len(missing) - 1; index >= 0; index-- {
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

func (platform *windowsOutputV3Platform) Root() outputcap.Directory {
	if platform == nil || platform.native == nil || platform.root == nil || platform.root.native == nil {
		return nil
	}
	return platform.root
}

func (platform *windowsOutputV3Platform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	if platform == nil || platform.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows output platform is closed"))
	}
	guard, err := platform.native.acquirePublicOperationGuard()
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	root := guard.Root()
	if root == nil {
		return nil, errors.Join(
			outputcap.ErrUnsafeNamespace,
			windowsOutputV3Error(guard.Close()),
			errors.New("osfs: Windows ancestry guard has no root authority"),
		)
	}
	return &windowsOutputV3PublicOperationGuard{
		native: guard,
		root:   &windowsOutputV3Directory{native: root},
	}, nil
}

func (guard *windowsOutputV3PublicOperationGuard) Root() outputcap.Directory {
	if guard == nil || guard.native == nil || guard.root == nil || guard.root.native == nil {
		return nil
	}
	return guard.root
}

func (guard *windowsOutputV3PublicOperationGuard) Close() error {
	if guard == nil || guard.native == nil {
		return nil
	}
	err := guard.native.Close()
	guard.native = nil
	if guard.root != nil {
		guard.root.native = nil
	}
	guard.root = nil
	return windowsOutputV3Error(err)
}

func (*windowsOutputV3Platform) Certification() resumestate.CertificationID {
	return resumestate.CertificationWindowsNTFSProcessRestart
}

func (platform *windowsOutputV3Platform) RootBinding() (
	_ resumestate.OutputRootBinding,
	resultErr error,
) {
	if platform == nil || platform.native == nil || platform.native.root == nil {
		return resumestate.OutputRootBinding{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows output platform is closed"),
		)
	}
	guard, err := platform.native.acquirePublicOperationGuard()
	if err != nil {
		return resumestate.OutputRootBinding{}, windowsOutputV3Error(err)
	}
	defer func() { resultErr = errors.Join(resultErr, windowsOutputV3Error(guard.Close())) }()
	root := guard.Root()
	if root == nil {
		return resumestate.OutputRootBinding{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows ancestry guard has no root authority"),
		)
	}
	if _, err := root.prepareIdentityClaim(); err != nil {
		return resumestate.OutputRootBinding{}, windowsOutputV3Error(err)
	}
	facts, err := root.inspector.Inspect(root.handle())
	if err != nil {
		return resumestate.OutputRootBinding{}, windowsOutputV3Error(
			windowsV3Failure("bind output root", root.path, errWindowsV3OutputUnsafe, err),
		)
	}
	if err := windowsV3ValidateOpenedObject(facts, root.volume, true); err != nil {
		return resumestate.OutputRootBinding{}, windowsOutputV3Error(
			windowsV3Failure("bind output root", root.path, errWindowsV3OutputUnsafe, err),
		)
	}
	objectID, prepared := root.objectIDState.current()
	if !prepared {
		return resumestate.OutputRootBinding{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows output-root Object ID was not prepared"),
		)
	}
	guid := strings.ToLower(facts.object.volume.guid)
	if len(guid) == 0 || len(guid) > windowsV3VolumeGUIDClaimMaxBytes {
		return resumestate.OutputRootBinding{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows volume GUID identity exceeds the bounded root-binding format"),
		)
	}
	volume := make([]byte, len("windows/ntfs/volume/v1")+4+len(guid)+8)
	copy(volume, "windows/ntfs/volume/v1")
	offset := len("windows/ntfs/volume/v1")
	binary.BigEndian.PutUint32(volume[offset:], uint32(len(guid)))
	offset += 4
	copy(volume[offset:], guid)
	offset += len(guid)
	binary.BigEndian.PutUint64(volume[offset:], facts.object.volume.serial)

	object := make([]byte, len("windows/ntfs/directory-object/v2")+len(objectID))
	copy(object, "windows/ntfs/directory-object/v2")
	copy(object[len("windows/ntfs/directory-object/v2"):], objectID[:])
	binding, err := resumestate.NewOutputRootBinding(platform.Certification(), volume, object)
	return binding, windowsOutputV3Error(err)
}

func (*windowsOutputV3Platform) Durability() transfer.DurabilityLevel {
	return transfer.DurabilityProcessRestart
}

func (platform *windowsOutputV3Platform) ProbeRecoverableFeatures() error {
	if platform == nil || platform.native == nil || platform.native.root == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows output platform is closed"))
	}
	guard, err := platform.native.acquirePublicOperationGuard()
	if err != nil {
		return windowsOutputV3Error(err)
	}
	probeErr := guard.Root().probeRecoverableFeatures()
	return errors.Join(windowsOutputV3Error(probeErr), windowsOutputV3Error(guard.Close()))
}

func (platform *windowsOutputV3Platform) ValidateSelectionMetadata(selection transfer.OutputSelection) error {
	if platform == nil || platform.native == nil || platform.native.root == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows output platform is closed"))
	}
	guard, err := platform.native.acquirePublicOperationGuard()
	if err != nil {
		return windowsOutputV3Error(err)
	}
	root := guard.Root()
	if root == nil {
		return errors.Join(
			outputcap.ErrUnsafeNamespace,
			windowsOutputV3Error(guard.Close()),
			errors.New("osfs: Windows metadata admission guard has no root authority"),
		)
	}
	validateErr := root.validateSelectionMetadata(selection)
	return errors.Join(windowsOutputV3Error(validateErr), windowsOutputV3Error(guard.Close()))
}

func (*windowsOutputV3Platform) ValidateModifiedTime(modified catalog.ModifiedTime) error {
	return windowsOutputV3Error(windowsV3ValidateModifiedTime(modified))
}

func (*windowsOutputV3Platform) CanonicalLocatorKey(path string) (string, error) {
	key, err := windowsV3OutputLocatorKey(path)
	return key, windowsOutputV3Error(err)
}

func (*windowsOutputV3Platform) CanonicalComponentKey(name string) (string, error) {
	native, err := windowsV3RelativePath(name, true)
	if err != nil {
		return "", windowsOutputV3Error(err)
	}
	key, err := windowsV3NTFSCaseKey(native)
	return key, windowsOutputV3Error(err)
}

func (platform *windowsOutputV3Platform) Close() error {
	if platform == nil || platform.native == nil {
		return nil
	}
	err := platform.native.Close()
	platform.native = nil
	if platform.root != nil {
		platform.root.native = nil
	}
	platform.root = nil
	return windowsOutputV3Error(err)
}
