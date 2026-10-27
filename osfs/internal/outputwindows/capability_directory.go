//go:build windows

package outputwindows

import (
	"errors"
	"io/fs"
	"math"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

func (directory *windowsOutputDirectory) PersistentDirectoryIdentityClaim() ([]byte, error) {
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
	return claim, windowsOutputError(err)
}

func (directory *windowsOutputDirectory) PreparePersistentDirectoryIdentityClaim() ([]byte, error) {
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
	return claim, windowsOutputError(err)
}

func (directory *windowsOutputDirectory) Close() error {
	if directory == nil || directory.native == nil {
		return nil
	}
	err := directory.native.Close()
	directory.native = nil
	return windowsOutputError(err)
}

func (directory *windowsOutputDirectory) Duplicate() (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	native, err := directory.native.Duplicate()
	if err != nil {
		return nil, windowsOutputError(err)
	}
	return &windowsOutputDirectory{native: native}, nil
}

func (directory *windowsOutputDirectory) Sync() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	return windowsOutputError(directory.native.Sync())
}

func (directory *windowsOutputDirectory) Names(limit int) ([]string, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	names, err := directory.native.names(limit)
	return names, windowsOutputError(err)
}

func (directory *windowsOutputDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if directory == nil || directory.native == nil {
		return outputcap.EntryAbsent, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	kind, err := directory.native.observeEntry(name)
	return kind, windowsOutputError(err)
}

func (directory *windowsOutputDirectory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	if directory == nil || directory.native == nil {
		return outputcap.EntryAbsent, false, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows directory authority is closed"),
		)
	}
	kind, exact, err := directory.native.classifyExactEntry(name)
	return kind, exact, windowsOutputError(err)
}

func (directory *windowsOutputDirectory) ValidatePublicEntryNames(names []string) error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	return windowsOutputError(directory.native.validatePublicEntryNames(names))
}

func (directory *windowsOutputDirectory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows directory authority is closed"),
		)
	}
	pinned, err := directory.native.openPinnedEntry(name)
	if err != nil {
		return nil, windowsOutputError(err)
	}
	return &windowsOutputEntryRef{native: pinned}, nil
}

func (directory *windowsOutputDirectory) EntryMatches(
	name string,
	expected outputcap.CurrentEntryReference,
) (bool, error) {
	pinned, ok := expected.(*windowsOutputEntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows pinned entry"))
	}
	matches, err := directory.native.pinnedEntryMatches(name, pinned.native)
	return matches, windowsOutputError(err)
}

func (directory *windowsOutputDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	pinned, ok := expected.(*windowsOutputEntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows pinned directory"))
	}
	opened, err := directory.native.openPinnedDirectory(pinned.native, private)
	if err != nil {
		return nil, windowsOutputError(err)
	}
	return &windowsOutputDirectory{native: opened}, nil
}

func (directory *windowsOutputDirectory) RemoveEntry(name string, expected outputcap.CurrentEntryReference) error {
	pinned, ok := expected.(*windowsOutputEntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows pinned entry removal"))
	}
	return windowsOutputError(directory.native.removePinnedEntry(name, pinned.native))
}

func (directory *windowsOutputDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	right, ok := other.(*windowsOutputDirectory)
	if !ok || directory == nil || directory.native == nil || right == nil || right.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows directory authority"))
	}
	same, err := sameWindowsOpenedDirectory(directory.native, right.native)
	return same, windowsOutputError(err)
}

func (directory *windowsOutputDirectory) ValidateMetadataAuthority() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	if directory.native.private || directory.native.metadataHandleOpen {
		return nil
	}
	if directory.native.metadataOpenErr != nil {
		return windowsOutputError(directory.native.metadataOpenErr)
	}
	return windowsOutputError(windowsNativeOperationFailure(
		"validate output directory metadata authority", directory.native.path, windows.ERROR_ACCESS_DENIED,
	))
}

func (directory *windowsOutputDirectory) SetModifiedTime(modified catalog.ModifiedTime) error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	return windowsOutputError(directory.native.setModifiedTime(modified))
}

func (directory *windowsOutputDirectory) MetadataMatches(modified catalog.ModifiedTime) (bool, error) {
	if directory == nil || directory.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	if err := directory.native.verify(false); err != nil {
		return false, windowsOutputError(err)
	}
	metadata, err := windowsReadHandleMetadata(directory.native.handle())
	if err != nil {
		return false, windowsOutputError(windowsFailure(
			"inspect output directory metadata", directory.native.path, errWindowsOutputUnsafe, err,
		))
	}
	return windowsModifiedTimeMatches(metadata.modifiedTicks, modified), nil
}

func (directory *windowsOutputDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	var native *windowsDirectory
	var err error
	if private {
		native, err = directory.native.OpenPrivateDirectory(name)
	} else {
		native, err = directory.native.OpenDirectory(name)
	}
	if err != nil {
		return nil, windowsOutputError(err)
	}
	return &windowsOutputDirectory{native: native}, nil
}

func (directory *windowsOutputDirectory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	var native *windowsDirectory
	var err error
	if private {
		native, err = directory.native.CreatePrivateDirectory(name)
	} else {
		native, err = directory.native.openDirectory(name, false, windows.FILE_CREATE)
	}
	if err != nil {
		return nil, windowsOutputError(err)
	}
	return &windowsOutputDirectory{native: native}, nil
}

func (directory *windowsOutputDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	source, ok := candidate.(*windowsOutputDirectory)
	if !ok || directory == nil || directory.native == nil || source == nil || source.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows directory installation authority"))
	}
	installed, err := directory.native.InstallPrivateDirectoryNoReplace(source.native, name)
	if err != nil {
		return nil, windowsOutputError(err)
	}
	return &windowsOutputDirectory{native: installed}, nil
}

func (directory *windowsOutputDirectory) RemoveDirectory(name string, expected outputcap.Directory) error {
	target, ok := expected.(*windowsOutputDirectory)
	if !ok || directory == nil || directory.native == nil || target == nil || target.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows directory removal authority"))
	}
	return windowsOutputError(directory.native.RemoveDirectory(name, target.native))
}

func (directory *windowsOutputDirectory) CreateFile(name string, private bool, size int64) (outputcap.MutableFile, error) {
	if directory == nil || directory.native == nil || size < 0 {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: invalid Windows file creation authority or size"))
	}
	native, err := directory.native.createExactFile(name, private, size)
	if err != nil {
		return nil, windowsOutputError(err)
	}
	return newWindowsOutputMutableFile(native, private), nil
}

func (directory *windowsOutputDirectory) OpenObservedFile(name string, private bool) (outputcap.ObservedFile, error) {
	state, err := directory.openFileState(name, private, windowsReadFileAccess())
	if err != nil {
		return nil, err
	}
	return &windowsOutputObservedFile{State: state}, nil
}

func (directory *windowsOutputDirectory) OpenRecoveryDurabilityFile(
	name string,
	private bool,
) (outputcap.RecoveryDurabilityFile, error) {
	state, err := directory.openFileState(name, private, windowsRecoveryDurabilityFileAccess())
	if err != nil {
		return nil, err
	}
	return &windowsOutputRecoveryDurabilityFile{State: state}, nil
}

func (directory *windowsOutputDirectory) OpenMutableFile(name string, private bool) (outputcap.MutableFile, error) {
	state, err := directory.openFileState(name, private, windowsPrivateFileAccess())
	if err != nil {
		return nil, err
	}
	return &windowsOutputMutableFile{State: state}, nil
}

func (directory *windowsOutputDirectory) openFileState(
	name string,
	private bool,
	access uint32,
) (*windowsOutputFileState, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	native, _, err := directory.native.openFile(name, windows.FILE_OPEN, access, nil, private)
	if err != nil {
		return nil, windowsOutputError(err)
	}
	return &windowsOutputFileState{native: native, private: private}, nil
}

func (directory *windowsOutputDirectory) PublishFileNoReplace(
	source outputcap.FileIdentity,
	name string,
) (outputcap.PublishNoReplaceOutcome, error) {
	file, ok := windowsOutputFileStateFrom(source)
	if !ok || directory == nil || directory.native == nil || directory.native.private ||
		file.native == nil || file.private {
		return 0, errors.Join(
			outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows file publication authority"))
	}
	linked, err := directory.native.LinkRegularFileNoReplace(file.native, name)
	switch {
	case err == nil:
		if closeErr := linked.Close(); closeErr != nil {
			return outputcap.PublishNoReplaceIndeterminate, windowsOutputError(closeErr)
		}
		return outputcap.PublishNoReplaceCommitted, nil
	case errors.Is(err, errWindowsOutputCollision):
		return outputcap.PublishNoReplaceCollision, nil
	case windowsPublicationMayBeVisible(err):
		return outputcap.PublishNoReplaceIndeterminate, windowsOutputError(err)
	default:
		return 0, windowsOutputError(err)
	}
}

func (directory *windowsOutputDirectory) ReservePublicDirectoryNoReplace(
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
			return &windowsOutputDirectory{native: reserved}, outputcap.PublishNoReplaceIndeterminate,
				windowsOutputError(syncErr)
		}
		return &windowsOutputDirectory{native: reserved}, outputcap.PublishNoReplaceCommitted, nil
	case errors.Is(err, errWindowsOutputCollision):
		return nil, outputcap.PublishNoReplaceCollision, nil
	default:
		return nil, 0, windowsOutputError(err)
	}
}

func (directory *windowsOutputDirectory) CreateOrdinaryOutputStage(
	proofDirectory outputcap.Directory,
	name string,
	exactSize uint64,
) error {
	proof, ok := proofDirectory.(*windowsOutputDirectory)
	if !ok || directory == nil || directory.native == nil || directory.native.private ||
		proof == nil || proof.native == nil || !proof.native.private || name == "" ||
		exactSize > math.MaxInt64 {
		return errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: invalid Windows ordinary-output stage authority"))
	}
	stage, err := directory.native.createPublicInheritedDeleteOnCloseFile()
	if err != nil {
		return windowsOutputError(err)
	}
	defer func() { _ = stage.Close() }()
	if err := stage.Truncate(int64(exactSize)); err != nil {
		return windowsOutputError(err)
	}
	size, sizeErr := stage.Size()
	if sizeErr != nil || size != exactSize {
		return errors.Join(outputcap.ErrUnsafeNamespace, windowsOutputError(errors.Join(
			errors.New("osfs: Windows ordinary-output stage did not preserve its exact size"), sizeErr)))
	}
	return windowsOutputError(directory.native.moveLiveStageNoReplace(stage, proof.native, name))
}

// CreateProcessStage preserves final-parent inheritance while returning the
// original live handle. No persisted ticket is needed to own this exact object.
func (directory *windowsOutputDirectory) CreateProcessStage(
	stageDirectory outputcap.Directory,
	name string,
	size int64,
) (outputcap.MutableFile, error) {
	proof, ok := stageDirectory.(*windowsOutputDirectory)
	if !ok || directory == nil || directory.native == nil || directory.native.private ||
		proof == nil || proof.native == nil || !proof.native.private || name == "" || size < 0 {
		return nil, outputcap.ErrUnsafeNamespace
	}
	stage, err := directory.native.createPublicInheritedDeleteOnCloseFile()
	if err != nil {
		return nil, windowsOutputError(err)
	}
	if err := stage.Truncate(size); err != nil {
		return nil, windowsOutputError(errors.Join(err, stage.Close()))
	}
	if err := directory.native.moveLiveStageNoReplace(stage, proof.native, name); err != nil {
		// A late failure can follow installation. Preserve the original object
		// witness so the process owner can clean only its exact stage.
		return newWindowsOutputMutableFile(stage, false), windowsOutputError(err)
	}
	return newWindowsOutputMutableFile(stage, false), nil
}

func (directory *windowsOutputDirectory) CreateLiveCleanupStage(
	proofDirectory outputcap.Directory,
	ticket checkpointmodel.LiveCleanupTicket,
) error {
	proof, ok := proofDirectory.(*windowsOutputDirectory)
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
		return windowsOutputError(err)
	}
	defer func() { _ = stage.Close() }()
	if err := stage.Truncate(int64(ticket.ExactSize())); err != nil {
		return windowsOutputError(err)
	}
	size, sizeErr := stage.Size()
	if sizeErr != nil || size != ticket.ExactSize() {
		return errors.Join(outputcap.ErrUnsafeNamespace, windowsOutputError(errors.Join(
			errors.New("osfs: Windows live-cleanup stage did not preserve its exact size"), sizeErr)))
	}
	return windowsOutputError(directory.native.moveLiveStageNoReplace(stage, proof.native, ticket.StageName()))
}

func (directory *windowsOutputDirectory) RemoveLiveCleanupStage(
	ticket checkpointmodel.LiveCleanupTicket,
	expected outputcap.FileIdentity,
) error {
	file, ok := windowsOutputFileStateFrom(expected)
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
		return errors.Join(outputcap.ErrUnsafeNamespace, windowsOutputError(errors.Join(
			errors.New("osfs: Windows live-cleanup stage facts do not match its ticket"), err)))
	}
	if err := file.native.verify(false); err != nil {
		return windowsOutputError(err)
	}
	if err := windowsVerifyOpenedLeafAuthority(file.native.handle(), ticket.StageName(), true); err != nil {
		return windowsOutputError(err)
	}
	return windowsOutputError(errors.Join(
		directory.native.RemoveOrdinaryProfileLink(ticket.StageName(), file.native),
		directory.native.Sync(),
	))
}

func (directory *windowsOutputDirectory) LinkFileNoReplace(
	source outputcap.FileIdentity,
	name string,
) (outputcap.ObservedFile, error) {
	file, ok := windowsOutputFileStateFrom(source)
	if !ok || directory == nil || directory.native == nil || file.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows file link authority"))
	}
	linked, err := directory.native.LinkRegularFileNoReplace(file.native, name)
	if err != nil {
		return nil, windowsOutputError(err)
	}
	return newWindowsOutputObservedFile(linked, file.private), nil
}

func (directory *windowsOutputDirectory) ReplacePrivateFile(source outputcap.FileIdentity, name string) error {
	file, ok := windowsOutputFileStateFrom(source)
	if !ok || directory == nil || directory.native == nil || file.native == nil || !file.private {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows private state replacement authority"))
	}
	return windowsOutputError(directory.native.AtomicReplacePrivateFile(file.native, name))
}

func (directory *windowsOutputDirectory) RemoveFile(name string, expected outputcap.FileIdentity) error {
	file, ok := windowsOutputFileStateFrom(expected)
	if !ok || directory == nil || directory.native == nil || file.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows file removal authority"))
	}
	if file.private {
		return windowsOutputError(directory.native.RemoveRegularLink(name, file.native))
	}
	return windowsOutputError(directory.native.RemoveOrdinaryProfileLink(name, file.native))
}

func (entry *windowsOutputEntryRef) Kind() outputcap.EntryKind {
	if entry == nil || entry.native == nil {
		return outputcap.EntryAbsent
	}
	return entry.native.kind
}

func (entry *windowsOutputEntryRef) Close() error {
	if entry == nil || entry.native == nil {
		return nil
	}
	err := entry.native.close()
	entry.native = nil
	return windowsOutputError(err)
}

func (directory *windowsOutputDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if directory == nil || directory.native == nil {
		return nil, false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	if existingOnly {
		lock, err := directory.native.AcquireExistingStableLock(name)
		if err != nil {
			return nil, false, windowsOutputError(err)
		}
		return newWindowsOutputLock(lock), false, nil
	}
	lock, created, err := directory.native.AcquireStableLock(name)
	if err != nil {
		return nil, false, windowsOutputError(err)
	}
	return newWindowsOutputLock(lock), created, nil
}

func windowsOutputError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, errWindowsOutputUnsupported):
		return errors.Join(outputcap.ErrRecoverableOutputUnsupported, err)
	case errors.Is(err, errWindowsOutputUnsafe):
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	case errors.Is(err, errWindowsOutputCollision):
		return errors.Join(outputcap.ErrNamespaceCollision, err)
	case errors.Is(err, errWindowsOutputLockBusy):
		return errors.Join(outputcap.ErrNamespaceLockBusy, err)
	case errors.Is(err, fs.ErrExist):
		return errors.Join(outputcap.ErrNamespaceCollision, err)
	default:
		return err
	}
}

var (
	_ outputcap.Platform                            = (*windowsOutputPlatform)(nil)
	_ outputcap.Directory                           = (*windowsOutputDirectory)(nil)
	_ outputcap.MetadataAuthorityValidator          = (*windowsOutputDirectory)(nil)
	_ outputcap.PersistentDirectoryIdentity         = (*windowsOutputDirectory)(nil)
	_ outputcap.PersistentDirectoryIdentityPreparer = (*windowsOutputDirectory)(nil)
	_ outputcap.ObservedFile                        = (*windowsOutputObservedFile)(nil)
	_ outputcap.RecoveryDurabilityFile              = (*windowsOutputRecoveryDurabilityFile)(nil)
	_ outputcap.MutableFile                         = (*windowsOutputMutableFile)(nil)
	_ outputcap.Lock                                = (*windowsOutputLock)(nil)
)
