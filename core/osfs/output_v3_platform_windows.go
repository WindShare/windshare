//go:build windows

package osfs

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/windshare/windshare/core/catalog"
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

func openOutputV3Platform(path string, create bool) (outputV3Platform, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.Join(errOutputV3Unsafe,
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
	candidate := path
	missing := make([]string, 0, 4)
	var ancestor *windowsV3OutputPlatform
	for {
		opened, err := openWindowsV3OutputPlatform(candidate)
		if err == nil {
			ancestor = opened
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return nil, errors.Join(windowsV3Failure(operation, path, errWindowsV3OutputUnsafe,
				errors.New("no existing certified ancestor contains the requested root")), err)
		}
		missing = append(missing, filepath.Base(candidate))
		candidate = parent
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
	if observer != nil {
		if err := observer.ObserveOutputRootCreate(current.path, windowsV3OutputRootCreatePlacementPinned); err != nil {
			return nil, err
		}
	}
	for index := len(missing) - 1; index >= 0; index-- {
		created, err := current.openDirectory(missing[index], false, windows.FILE_CREATE)
		if err != nil {
			return nil, err
		}
		if err := errors.Join(created.Sync(), current.Sync()); err != nil {
			return nil, errors.Join(err, created.Close())
		}
		createdAuthorities = append(createdAuthorities, created)
		current = created
		if observer != nil {
			if err := observer.ObserveOutputRootCreate(current.path, windowsV3OutputRootCreateComponentPinned); err != nil {
				return nil, err
			}
		}
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

func (platform *windowsOutputV3Platform) Root() outputV3Directory {
	if platform == nil || platform.native == nil || platform.root == nil || platform.root.native == nil {
		return nil
	}
	return platform.root
}

func (platform *windowsOutputV3Platform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	if platform == nil || platform.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows output platform is closed"))
	}
	guard, err := platform.native.acquirePublicOperationGuard()
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	root := guard.Root()
	if root == nil {
		return nil, errors.Join(
			errOutputV3Unsafe,
			windowsOutputV3Error(guard.Close()),
			errors.New("osfs: Windows ancestry guard has no root authority"),
		)
	}
	return &windowsOutputV3PublicOperationGuard{
		native: guard,
		root:   &windowsOutputV3Directory{native: root},
	}, nil
}

func (guard *windowsOutputV3PublicOperationGuard) Root() outputV3Directory {
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
			errOutputV3Unsafe,
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
			errOutputV3Unsafe,
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
			errOutputV3Unsafe,
			errors.New("osfs: Windows output-root Object ID was not prepared"),
		)
	}
	guid := strings.ToLower(facts.object.volume.guid)
	if len(guid) == 0 || len(guid) > windowsV3VolumeGUIDClaimMaxBytes {
		return resumestate.OutputRootBinding{}, errors.Join(
			errOutputV3Unsafe,
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
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows output platform is closed"))
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
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows output platform is closed"))
	}
	guard, err := platform.native.acquirePublicOperationGuard()
	if err != nil {
		return windowsOutputV3Error(err)
	}
	root := guard.Root()
	if root == nil {
		return errors.Join(
			errOutputV3Unsafe,
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

func (directory *windowsOutputV3Directory) Close() error {
	if directory == nil || directory.native == nil {
		return nil
	}
	err := directory.native.Close()
	directory.native = nil
	return windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) Duplicate() (outputV3Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	native, err := directory.native.Duplicate()
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Directory{native: native}, nil
}

func (directory *windowsOutputV3Directory) Sync() error {
	if directory == nil || directory.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	return windowsOutputV3Error(directory.native.Sync())
}

func (directory *windowsOutputV3Directory) Names(limit int) ([]string, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	names, err := directory.native.names(limit)
	return names, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) NamesWithPrefix(prefix string, matchLimit int) ([]string, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	names, err := directory.native.namesWithPrefix(prefix, matchLimit)
	return names, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) ObserveEntry(name string) (outputV3EntryKind, error) {
	if directory == nil || directory.native == nil {
		return outputV3EntryAbsent, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	kind, err := directory.native.observeEntry(name)
	return kind, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) ClassifyExactEntry(name string) (outputV3EntryKind, bool, error) {
	if directory == nil || directory.native == nil {
		return outputV3EntryAbsent, false, errors.Join(
			errOutputV3Unsafe,
			errors.New("osfs: Windows directory authority is closed"),
		)
	}
	kind, exact, err := directory.native.classifyExactEntry(name)
	return kind, exact, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) ValidatePublicEntryName(name string) error {
	if directory == nil || directory.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	return windowsOutputV3Error(directory.native.validatePublicEntryName(name))
}

func (directory *windowsOutputV3Directory) ValidatePublicEntryNames(names []string) error {
	if directory == nil || directory.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	return windowsOutputV3Error(directory.native.validatePublicEntryNames(names))
}

func (directory *windowsOutputV3Directory) OpenEntry(name string) (outputV3EntryRef, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(
			errOutputV3Unsafe,
			errors.New("osfs: Windows directory authority is closed"),
		)
	}
	pinned, err := directory.native.openPinnedEntry(name)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3EntryRef{native: pinned}, nil
}

func (directory *windowsOutputV3Directory) EntryMatches(
	name string,
	expected outputV3EntryRef,
) (bool, error) {
	pinned, ok := expected.(*windowsOutputV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return false, errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Windows pinned entry"))
	}
	matches, err := directory.native.pinnedEntryMatches(name, pinned.native)
	return matches, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) OpenPinnedDirectory(
	expected outputV3EntryRef,
	private bool,
) (outputV3Directory, error) {
	pinned, ok := expected.(*windowsOutputV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Windows pinned directory"))
	}
	opened, err := directory.native.openPinnedDirectory(pinned.native, private)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Directory{native: opened}, nil
}

func (directory *windowsOutputV3Directory) RemoveEntry(name string, expected outputV3EntryRef) error {
	pinned, ok := expected.(*windowsOutputV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Windows pinned entry removal"))
	}
	return windowsOutputV3Error(directory.native.removePinnedEntry(name, pinned.native))
}

func (directory *windowsOutputV3Directory) IdentityClaim() ([]byte, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	claim, err := directory.native.identityClaim()
	return claim, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) PrepareIdentityClaim() ([]byte, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	claim, err := directory.native.prepareIdentityClaim()
	return claim, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) SameDirectory(other outputV3Directory) (bool, error) {
	right, ok := other.(*windowsOutputV3Directory)
	if !ok || directory == nil || directory.native == nil || right == nil || right.native == nil {
		return false, errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Windows directory authority"))
	}
	same, err := sameWindowsV3OpenedDirectory(directory.native, right.native)
	return same, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) SetModifiedTime(modified catalog.ModifiedTime) error {
	if directory == nil || directory.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	return windowsOutputV3Error(directory.native.setModifiedTime(modified))
}

func (directory *windowsOutputV3Directory) OpenDirectory(name string, private bool) (outputV3Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	var native *windowsV3Directory
	var err error
	if private {
		native, err = directory.native.OpenPrivateDirectory(name)
	} else {
		native, err = directory.native.OpenDirectory(name)
	}
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Directory{native: native}, nil
}

func (directory *windowsOutputV3Directory) CreateDirectory(name string, private bool) (outputV3Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	var native *windowsV3Directory
	var err error
	if private {
		native, err = directory.native.CreatePrivateDirectory(name)
	} else {
		native, err = directory.native.openDirectory(name, false, windows.FILE_CREATE)
	}
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Directory{native: native}, nil
}

func (directory *windowsOutputV3Directory) InstallDirectoryNoReplace(
	candidate outputV3Directory,
	name string,
) (outputV3Directory, error) {
	source, ok := candidate.(*windowsOutputV3Directory)
	if !ok || directory == nil || directory.native == nil || source == nil || source.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Windows directory installation authority"))
	}
	installed, err := directory.native.InstallPrivateDirectoryNoReplace(source.native, name)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Directory{native: installed}, nil
}

func (directory *windowsOutputV3Directory) RemoveDirectory(name string, expected outputV3Directory) error {
	target, ok := expected.(*windowsOutputV3Directory)
	if !ok || directory == nil || directory.native == nil || target == nil || target.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Windows directory removal authority"))
	}
	return windowsOutputV3Error(directory.native.RemoveDirectory(name, target.native))
}

func (directory *windowsOutputV3Directory) CreateFile(name string, private bool, size int64) (outputV3File, error) {
	if directory == nil || directory.native == nil || size < 0 {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: invalid Windows file creation authority or size"))
	}
	var native *windowsV3File
	var err error
	if private {
		native, err = directory.native.CreatePrivateFile(name)
	} else {
		native, _, err = directory.native.openFile(
			name, windows.FILE_CREATE, windowsV3PrivateFileAccess(), nil, false,
		)
	}
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	if err := native.Truncate(size); err != nil {
		removeErr := directory.native.RemoveRegularLink(name, native)
		return nil, windowsOutputV3Error(errors.Join(err, removeErr, native.Close()))
	}
	actual, err := native.Size()
	if err != nil || actual != uint64(size) {
		removeErr := directory.native.RemoveRegularLink(name, native)
		return nil, windowsOutputV3Error(errors.Join(
			errors.New("osfs: Windows file creation did not install the exact size"), err, removeErr, native.Close(),
		))
	}
	return &windowsOutputV3File{native: native, private: private}, nil
}

func (directory *windowsOutputV3Directory) OpenFile(name string, private, writable bool) (outputV3File, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	access := windowsV3ReadFileAccess()
	if writable {
		access = windowsV3PrivateFileAccess()
	}
	native, _, err := directory.native.openFile(name, windows.FILE_OPEN, access, nil, private)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3File{native: native, private: private}, nil
}

func (directory *windowsOutputV3Directory) LinkFileNoReplace(source outputV3File, name string) (outputV3File, error) {
	file, ok := source.(*windowsOutputV3File)
	if !ok || directory == nil || directory.native == nil || file == nil || file.native == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Windows file link authority"))
	}
	linked, err := directory.native.LinkRegularFileNoReplace(file.native, name)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3File{native: linked}, nil
}

func (directory *windowsOutputV3Directory) ReplacePrivateFile(source outputV3File, name string) error {
	file, ok := source.(*windowsOutputV3File)
	if !ok || directory == nil || directory.native == nil || file == nil || file.native == nil || !file.private {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Windows private state replacement authority"))
	}
	return windowsOutputV3Error(directory.native.AtomicReplacePrivateFile(file.native, name))
}

func (directory *windowsOutputV3Directory) RemoveFile(name string, expected outputV3File) error {
	file, ok := expected.(*windowsOutputV3File)
	if !ok || directory == nil || directory.native == nil || file == nil || file.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Windows file removal authority"))
	}
	return windowsOutputV3Error(directory.native.RemoveRegularLink(name, file.native))
}

func (entry *windowsOutputV3EntryRef) Kind() outputV3EntryKind {
	if entry == nil || entry.native == nil {
		return outputV3EntryAbsent
	}
	return entry.native.kind
}

func (entry *windowsOutputV3EntryRef) AllocatedSize() (uint64, error) {
	if entry == nil || entry.native == nil {
		return 0, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows pinned entry is closed"))
	}
	size, err := entry.native.allocatedSize()
	return size, windowsOutputV3Error(err)
}

func (entry *windowsOutputV3EntryRef) Close() error {
	if entry == nil || entry.native == nil {
		return nil
	}
	err := entry.native.close()
	entry.native = nil
	return windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) AcquireLock(
	name string,
	existingOnly bool,
) (outputV3Lock, bool, error) {
	if directory == nil || directory.native == nil {
		return nil, false, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows directory authority is closed"))
	}
	if existingOnly {
		lock, err := directory.native.AcquireExistingStableLock(name)
		if err != nil {
			return nil, false, windowsOutputV3Error(err)
		}
		return newWindowsOutputV3Lock(lock), false, nil
	}
	lock, created, err := directory.native.AcquireStableLock(name)
	if err != nil {
		return nil, false, windowsOutputV3Error(err)
	}
	return newWindowsOutputV3Lock(lock), created, nil
}

func newWindowsOutputV3Lock(lock *windowsV3StableLock) *windowsOutputV3Lock {
	file := &windowsOutputV3File{private: true, borrowed: true}
	if lock != nil {
		file.native = lock.file
	}
	return &windowsOutputV3Lock{native: lock, file: file}
}

func (file *windowsOutputV3File) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows file authority is closed"))
	}
	count, err := file.native.ReadAt(destination, offset)
	return count, windowsOutputV3Error(err)
}

func (file *windowsOutputV3File) WriteAt(source []byte, offset int64) (int, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows file authority is closed"))
	}
	count, err := file.native.WriteAt(source, offset)
	return count, windowsOutputV3Error(err)
}

func (file *windowsOutputV3File) Close() error {
	if file == nil || file.borrowed || file.native == nil {
		return nil
	}
	err := file.native.Close()
	file.native = nil
	return windowsOutputV3Error(err)
}

func (file *windowsOutputV3File) Sync() error {
	if file == nil || file.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows file authority is closed"))
	}
	return windowsOutputV3Error(file.native.Sync())
}

func (file *windowsOutputV3File) Truncate(size int64) error {
	if file == nil || file.native == nil || size < 0 {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: invalid Windows file authority or size"))
	}
	if err := file.native.Truncate(size); err != nil {
		return windowsOutputV3Error(err)
	}
	actual, err := file.native.Size()
	if err != nil {
		return windowsOutputV3Error(err)
	}
	if actual != uint64(size) {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows file size differs after truncate"))
	}
	return nil
}

func (file *windowsOutputV3File) Size() (uint64, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows file authority is closed"))
	}
	size, err := file.native.Size()
	return size, windowsOutputV3Error(err)
}

func (file *windowsOutputV3File) AllocatedSize() (uint64, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows file authority is closed"))
	}
	size, err := file.native.allocatedSize()
	return size, windowsOutputV3Error(err)
}

func (file *windowsOutputV3File) SetModifiedTime(modified catalog.ModifiedTime) error {
	if file == nil || file.native == nil {
		return errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows file authority is closed"))
	}
	return windowsOutputV3Error(file.native.setModifiedTime(modified))
}

func (file *windowsOutputV3File) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if file == nil || file.native == nil {
		return false, errors.Join(errOutputV3Unsafe, errors.New("osfs: Windows file authority is closed"))
	}
	matches, err := file.native.metadataMatches(size, modified)
	return matches, windowsOutputV3Error(err)
}

func (file *windowsOutputV3File) SameFile(other outputV3File) (bool, error) {
	right, ok := other.(*windowsOutputV3File)
	if !ok || file == nil || file.native == nil || right == nil || right.native == nil {
		return false, errors.Join(errOutputV3Unsafe, errors.New("osfs: incompatible Windows file authority"))
	}
	same, err := sameWindowsV3OpenedObject(file.native, right.native)
	return same, windowsOutputV3Error(err)
}

func (lock *windowsOutputV3Lock) File() outputV3File {
	if lock == nil || lock.native == nil || lock.file == nil || lock.file.native == nil {
		return nil
	}
	return lock.file
}

func (lock *windowsOutputV3Lock) Close() error {
	if lock == nil || lock.native == nil {
		return nil
	}
	err := lock.native.Close()
	lock.native = nil
	if lock.file != nil {
		lock.file.native = nil
	}
	return windowsOutputV3Error(err)
}

func windowsOutputV3Error(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, errWindowsV3OutputUnsupported):
		return errors.Join(errOutputV3Unsupported, err)
	case errors.Is(err, errWindowsV3OutputUnsafe):
		return errors.Join(errOutputV3Unsafe, err)
	case errors.Is(err, errWindowsV3OutputCollision):
		return errors.Join(errOutputV3Collision, err)
	case errors.Is(err, errWindowsV3OutputLockBusy):
		return errors.Join(errOutputV3LockBusy, err)
	case errors.Is(err, fs.ErrExist):
		return errors.Join(errOutputV3Collision, err)
	default:
		return err
	}
}

var (
	_ outputV3Platform  = (*windowsOutputV3Platform)(nil)
	_ outputV3Directory = (*windowsOutputV3Directory)(nil)
	_ outputV3File      = (*windowsOutputV3File)(nil)
	_ outputV3Lock      = (*windowsOutputV3Lock)(nil)
)
