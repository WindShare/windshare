//go:build windows

package outputwindows

import (
	"errors"
	"io/fs"
	"math"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

func (directory *windowsOutputV3Directory) PersistentDirectoryIdentityClaim() ([]byte, error) {
	if directory == nil || directory.native == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	var claim []byte
	var err error
	if directory.native.private {
		claim, err = directory.native.privateIdentityClaim()
	} else {
		claim, err = directory.native.identityClaim()
	}
	return claim, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) PreparePersistentDirectoryIdentityClaim() ([]byte, error) {
	if directory == nil || directory.native == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	var claim []byte
	var err error
	if directory.native.private {
		claim, err = directory.native.preparePrivateIdentityClaim()
	} else {
		claim, err = directory.native.prepareIdentityClaim()
	}
	return claim, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) Close() error {
	if directory == nil || directory.native == nil {
		return nil
	}
	err := directory.native.Close()
	directory.native = nil
	return windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) Duplicate() (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	native, err := directory.native.Duplicate()
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Directory{native: native}, nil
}

func (directory *windowsOutputV3Directory) Sync() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	return windowsOutputV3Error(directory.native.Sync())
}

func (directory *windowsOutputV3Directory) Names(limit int) ([]string, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	names, err := directory.native.names(limit)
	return names, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if directory == nil || directory.native == nil {
		return outputcap.EntryAbsent, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	kind, err := directory.native.observeEntry(name)
	return kind, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	if directory == nil || directory.native == nil {
		return outputcap.EntryAbsent, false, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows directory authority is closed"),
		)
	}
	kind, exact, err := directory.native.classifyExactEntry(name)
	return kind, exact, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) ValidatePublicEntryNames(names []string) error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	return windowsOutputV3Error(directory.native.validatePublicEntryNames(names))
}

func (directory *windowsOutputV3Directory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(
			outputcap.ErrUnsafeNamespace,
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
	expected outputcap.CurrentEntryReference,
) (bool, error) {
	pinned, ok := expected.(*windowsOutputV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows pinned entry"))
	}
	matches, err := directory.native.pinnedEntryMatches(name, pinned.native)
	return matches, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	pinned, ok := expected.(*windowsOutputV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows pinned directory"))
	}
	opened, err := directory.native.openPinnedDirectory(pinned.native, private)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Directory{native: opened}, nil
}

func (directory *windowsOutputV3Directory) RemoveEntry(name string, expected outputcap.CurrentEntryReference) error {
	pinned, ok := expected.(*windowsOutputV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows pinned entry removal"))
	}
	return windowsOutputV3Error(directory.native.removePinnedEntry(name, pinned.native))
}

func (directory *windowsOutputV3Directory) SameDirectory(other outputcap.Directory) (bool, error) {
	right, ok := other.(*windowsOutputV3Directory)
	if !ok || directory == nil || directory.native == nil || right == nil || right.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows directory authority"))
	}
	same, err := sameWindowsV3OpenedDirectory(directory.native, right.native)
	return same, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) ValidateMetadataAuthority() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	if directory.native.private || directory.native.metadataHandleOpen {
		return nil
	}
	if directory.native.metadataOpenErr != nil {
		return windowsOutputV3Error(directory.native.metadataOpenErr)
	}
	return windowsOutputV3Error(windowsV3NativeOperationFailure(
		"validate output directory metadata authority", directory.native.path, windows.ERROR_ACCESS_DENIED,
	))
}

func (directory *windowsOutputV3Directory) SetModifiedTime(modified catalog.ModifiedTime) error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	return windowsOutputV3Error(directory.native.setModifiedTime(modified))
}

func (directory *windowsOutputV3Directory) MetadataMatches(modified catalog.ModifiedTime) (bool, error) {
	if directory == nil || directory.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	if err := directory.native.verify(false); err != nil {
		return false, windowsOutputV3Error(err)
	}
	metadata, err := windowsV3ReadHandleMetadata(directory.native.handle())
	if err != nil {
		return false, windowsOutputV3Error(windowsV3Failure(
			"inspect output directory metadata", directory.native.path, errWindowsV3OutputUnsafe, err,
		))
	}
	return windowsV3ModifiedTimeMatches(metadata.modifiedTicks, modified), nil
}

func (directory *windowsOutputV3Directory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
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

func (directory *windowsOutputV3Directory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
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
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	source, ok := candidate.(*windowsOutputV3Directory)
	if !ok || directory == nil || directory.native == nil || source == nil || source.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows directory installation authority"))
	}
	installed, err := directory.native.InstallPrivateDirectoryNoReplace(source.native, name)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Directory{native: installed}, nil
}

func (directory *windowsOutputV3Directory) RemoveDirectory(name string, expected outputcap.Directory) error {
	target, ok := expected.(*windowsOutputV3Directory)
	if !ok || directory == nil || directory.native == nil || target == nil || target.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows directory removal authority"))
	}
	return windowsOutputV3Error(directory.native.RemoveDirectory(name, target.native))
}

func (directory *windowsOutputV3Directory) CreateFile(name string, private bool, size int64) (outputcap.MutableFile, error) {
	if directory == nil || directory.native == nil || size < 0 {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: invalid Windows file creation authority or size"))
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
	return newWindowsOutputV3MutableFile(native, private), nil
}

func (directory *windowsOutputV3Directory) OpenObservedFile(name string, private bool) (outputcap.ObservedFile, error) {
	state, err := directory.openFileState(name, private, windowsV3ReadFileAccess())
	if err != nil {
		return nil, err
	}
	return &windowsOutputV3ObservedFile{state: state}, nil
}

func (directory *windowsOutputV3Directory) OpenRecoveryDurabilityFile(
	name string,
	private bool,
) (outputcap.RecoveryDurabilityFile, error) {
	state, err := directory.openFileState(name, private, windowsV3RecoveryDurabilityFileAccess())
	if err != nil {
		return nil, err
	}
	return &windowsOutputV3RecoveryDurabilityFile{state: state}, nil
}

func (directory *windowsOutputV3Directory) OpenMutableFile(name string, private bool) (outputcap.MutableFile, error) {
	state, err := directory.openFileState(name, private, windowsV3PrivateFileAccess())
	if err != nil {
		return nil, err
	}
	return &windowsOutputV3MutableFile{state: state}, nil
}

func (directory *windowsOutputV3Directory) openFileState(
	name string,
	private bool,
	access uint32,
) (*windowsOutputV3FileState, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	native, _, err := directory.native.openFile(name, windows.FILE_OPEN, access, nil, private)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3FileState{native: native, private: private}, nil
}

func (directory *windowsOutputV3Directory) PublishFileNoReplace(
	source outputcap.FileIdentity,
	name string,
) (outputcap.PublishNoReplaceOutcome, error) {
	file, ok := windowsOutputV3FileStateFrom(source)
	if !ok || directory == nil || directory.native == nil || directory.native.private ||
		file.native == nil || file.private {
		return 0, errors.Join(
			outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows file publication authority"))
	}
	linked, err := directory.native.LinkRegularFileNoReplace(file.native, name)
	switch {
	case err == nil:
		if closeErr := linked.Close(); closeErr != nil {
			return outputcap.PublishNoReplaceIndeterminate, windowsOutputV3Error(closeErr)
		}
		return outputcap.PublishNoReplaceCommitted, nil
	case errors.Is(err, errWindowsV3OutputCollision):
		return outputcap.PublishNoReplaceCollision, nil
	case windowsV3PublicationMayBeVisible(err):
		return outputcap.PublishNoReplaceIndeterminate, windowsOutputV3Error(err)
	default:
		return 0, windowsOutputV3Error(err)
	}
}

func (directory *windowsOutputV3Directory) ReservePublicDirectoryNoReplace(
	name string,
) (outputcap.Directory, outputcap.PublishNoReplaceOutcome, error) {
	if directory == nil || directory.native == nil || directory.native.private {
		return nil, 0, errors.Join(
			outputcap.ErrUnsafeNamespace, errors.New("osfs: invalid Windows public-directory reservation authority"))
	}
	reserved, err := directory.native.openDirectory(name, false, windows.FILE_CREATE)
	switch {
	case err == nil:
		if syncErr := directory.native.Sync(); syncErr != nil {
			return &windowsOutputV3Directory{native: reserved}, outputcap.PublishNoReplaceIndeterminate,
				windowsOutputV3Error(syncErr)
		}
		return &windowsOutputV3Directory{native: reserved}, outputcap.PublishNoReplaceCommitted, nil
	case errors.Is(err, errWindowsV3OutputCollision):
		return nil, outputcap.PublishNoReplaceCollision, nil
	default:
		return nil, 0, windowsOutputV3Error(err)
	}
}

func (directory *windowsOutputV3Directory) CreateOrdinaryOutputStage(
	proofDirectory outputcap.Directory,
	name string,
	exactSize uint64,
) error {
	proof, ok := proofDirectory.(*windowsOutputV3Directory)
	if !ok || directory == nil || directory.native == nil || directory.native.private ||
		proof == nil || proof.native == nil || !proof.native.private || name == "" ||
		exactSize > math.MaxInt64 {
		return errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: invalid Windows ordinary-output stage authority"))
	}
	stage, err := directory.native.createPublicInheritedDeleteOnCloseFile()
	if err != nil {
		return windowsOutputV3Error(err)
	}
	defer func() { _ = stage.Close() }()
	if err := stage.Truncate(int64(exactSize)); err != nil {
		return windowsOutputV3Error(err)
	}
	size, sizeErr := stage.Size()
	if sizeErr != nil || size != exactSize {
		return errors.Join(outputcap.ErrUnsafeNamespace, windowsOutputV3Error(errors.Join(
			errors.New("osfs: Windows ordinary-output stage did not preserve its exact size"), sizeErr)))
	}
	return windowsOutputV3Error(directory.native.moveLiveStageNoReplace(stage, proof.native, name))
}

func (directory *windowsOutputV3Directory) CreateLiveCleanupStage(
	proofDirectory outputcap.Directory,
	ticket checkpointmodel.LiveCleanupTicket,
) error {
	proof, ok := proofDirectory.(*windowsOutputV3Directory)
	if !ok || directory == nil || directory.native == nil || directory.native.private ||
		proof == nil || proof.native == nil || !proof.native.private ||
		!ticket.Valid() || ticket.Profile() != checkpointmodel.LiveCleanupWindowsNTFSV1 ||
		ticket.State() != checkpointmodel.LiveCleanupTicketCommitted ||
		ticket.ExactSize() > math.MaxInt64 {
		return errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: invalid Windows live-cleanup stage authority"))
	}
	stage, err := directory.native.createPublicInheritedDeleteOnCloseFile()
	if err != nil {
		return windowsOutputV3Error(err)
	}
	defer func() { _ = stage.Close() }()
	if err := stage.Truncate(int64(ticket.ExactSize())); err != nil {
		return windowsOutputV3Error(err)
	}
	size, sizeErr := stage.Size()
	if sizeErr != nil || size != ticket.ExactSize() {
		return errors.Join(outputcap.ErrUnsafeNamespace, windowsOutputV3Error(errors.Join(
			errors.New("osfs: Windows live-cleanup stage did not preserve its exact size"), sizeErr)))
	}
	return windowsOutputV3Error(directory.native.moveLiveStageNoReplace(stage, proof.native, ticket.StageName()))
}

func (directory *windowsOutputV3Directory) RemoveLiveCleanupStage(
	ticket checkpointmodel.LiveCleanupTicket,
	expected outputcap.FileIdentity,
) error {
	file, ok := windowsOutputV3FileStateFrom(expected)
	if !ok || directory == nil || directory.native == nil || !directory.native.private ||
		file.native == nil || !ticket.Valid() ||
		ticket.Profile() != checkpointmodel.LiveCleanupWindowsNTFSV1 {
		return errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: invalid Windows live-cleanup removal authority"))
	}
	if ticket.State() != checkpointmodel.LiveCleanupTicketCommitted &&
		ticket.State() != checkpointmodel.LiveCleanupStageCreated {
		return errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows live-cleanup ticket cannot own a stage"))
	}
	size, err := file.native.Size()
	if err != nil || size != ticket.ExactSize() {
		return errors.Join(outputcap.ErrUnsafeNamespace, windowsOutputV3Error(errors.Join(
			errors.New("osfs: Windows live-cleanup stage facts do not match its ticket"), err)))
	}
	if err := file.native.verify(false); err != nil {
		return windowsOutputV3Error(err)
	}
	if err := windowsV3VerifyOpenedLeafAuthority(file.native.handle(), ticket.StageName(), true); err != nil {
		return windowsOutputV3Error(err)
	}
	return windowsOutputV3Error(errors.Join(
		directory.native.RemoveOrdinaryProfileLink(ticket.StageName(), file.native),
		directory.native.Sync(),
	))
}

func (directory *windowsOutputV3Directory) LinkFileNoReplace(
	source outputcap.FileIdentity,
	name string,
) (outputcap.ObservedFile, error) {
	file, ok := windowsOutputV3FileStateFrom(source)
	if !ok || directory == nil || directory.native == nil || file.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows file link authority"))
	}
	linked, err := directory.native.LinkRegularFileNoReplace(file.native, name)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return newWindowsOutputV3ObservedFile(linked, file.private), nil
}

func (directory *windowsOutputV3Directory) ReplacePrivateFile(source outputcap.FileIdentity, name string) error {
	file, ok := windowsOutputV3FileStateFrom(source)
	if !ok || directory == nil || directory.native == nil || file.native == nil || !file.private {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows private state replacement authority"))
	}
	return windowsOutputV3Error(directory.native.AtomicReplacePrivateFile(file.native, name))
}

func (directory *windowsOutputV3Directory) RemoveFile(name string, expected outputcap.FileIdentity) error {
	file, ok := windowsOutputV3FileStateFrom(expected)
	if !ok || directory == nil || directory.native == nil || file.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows file removal authority"))
	}
	if file.private {
		return windowsOutputV3Error(directory.native.RemoveRegularLink(name, file.native))
	}
	return windowsOutputV3Error(directory.native.RemoveOrdinaryProfileLink(name, file.native))
}

func (entry *windowsOutputV3EntryRef) Kind() outputcap.EntryKind {
	if entry == nil || entry.native == nil {
		return outputcap.EntryAbsent
	}
	return entry.native.kind
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
) (outputcap.Lock, bool, error) {
	if directory == nil || directory.native == nil {
		return nil, false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
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

func windowsOutputV3Error(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, errWindowsV3OutputUnsupported):
		return errors.Join(outputcap.ErrRecoverableOutputUnsupported, err)
	case errors.Is(err, errWindowsV3OutputUnsafe):
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	case errors.Is(err, errWindowsV3OutputCollision):
		return errors.Join(outputcap.ErrNamespaceCollision, err)
	case errors.Is(err, errWindowsV3OutputLockBusy):
		return errors.Join(outputcap.ErrNamespaceLockBusy, err)
	case errors.Is(err, fs.ErrExist):
		return errors.Join(outputcap.ErrNamespaceCollision, err)
	default:
		return err
	}
}

var (
	_ outputcap.Platform                            = (*windowsOutputV3Platform)(nil)
	_ outputcap.Directory                           = (*windowsOutputV3Directory)(nil)
	_ outputcap.MetadataAuthorityValidator          = (*windowsOutputV3Directory)(nil)
	_ outputcap.PersistentDirectoryIdentity         = (*windowsOutputV3Directory)(nil)
	_ outputcap.PersistentDirectoryIdentityPreparer = (*windowsOutputV3Directory)(nil)
	_ outputcap.ObservedFile                        = (*windowsOutputV3ObservedFile)(nil)
	_ outputcap.RecoveryDurabilityFile              = (*windowsOutputV3RecoveryDurabilityFile)(nil)
	_ outputcap.MutableFile                         = (*windowsOutputV3MutableFile)(nil)
	_ outputcap.Lock                                = (*windowsOutputV3Lock)(nil)
)
