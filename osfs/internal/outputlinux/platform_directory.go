//go:build linux

package outputlinux

import (
	"errors"
	"math"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func (directory *linuxDirectory) PersistentDirectoryIdentityClaim() ([]byte, error) {
	if directory == nil || directory.native == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	claim, err := directory.native.identityClaim()
	return claim, linuxError(err)
}

func (directory *linuxDirectory) PreparePersistentDirectoryIdentityClaim() ([]byte, error) {
	if directory == nil || directory.native == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	claim, err := directory.native.prepareIdentityClaim()
	return claim, linuxError(err)
}

func (directory *linuxDirectory) Close() error {
	if directory == nil {
		return nil
	}
	var originErr error
	if directory.origin != nil && directory.origin.parent != nil {
		originErr = directory.origin.parent.close()
	}
	directory.origin = nil
	var nativeErr error
	if directory.native != nil {
		nativeErr = directory.native.close()
	}
	directory.native = nil
	return linuxError(errors.Join(originErr, nativeErr))
}

func (directory *linuxDirectory) Duplicate() (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	native, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxError(err)
	}
	result := &linuxDirectory{native: native}
	if directory.origin != nil {
		parent, duplicateErr := directory.origin.parent.Duplicate()
		if duplicateErr != nil {
			return nil, linuxError(errors.Join(duplicateErr, native.close()))
		}
		result.origin = &linuxDirectoryOrigin{parent: parent, name: directory.origin.name}
	}
	return result, nil
}

func (directory *linuxDirectory) Sync() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	return linuxError(directory.native.sync())
}

func (directory *linuxDirectory) Names(limit int) ([]string, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	names, err := directory.native.names(limit)
	return names, linuxError(err)
}

func (directory *linuxDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if directory == nil || directory.native == nil {
		return outputcap.EntryAbsent, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	kind, err := directory.native.observeEntry(name)
	return kind, linuxError(err)
}

func (directory *linuxDirectory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	if directory == nil || directory.native == nil {
		return outputcap.EntryAbsent, false, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Linux directory authority is closed"),
		)
	}
	kind, exact, err := directory.native.classifyExactEntry(name)
	return kind, exact, linuxError(err)
}

func (directory *linuxDirectory) validatePublicEntryName(name string) error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	_, exact, err := directory.native.classifyExactEntry(name)
	if err != nil {
		return linuxError(err)
	}
	if !exact {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}

func (directory *linuxDirectory) ValidatePublicEntryNames(names []string) error {
	for _, name := range names {
		if err := directory.validatePublicEntryName(name); err != nil {
			return err
		}
	}
	return nil
}

func (directory *linuxDirectory) ValidateCreateAuthority() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	return linuxError(directory.native.validateCreateAuthority())
}

func (directory *linuxDirectory) ValidateMetadataAuthority() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	if err := directory.native.validateMetadataAuthority(); err != nil {
		return linuxError(err)
	}
	return nil
}

func (directory *linuxDirectory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	pinned, err := directory.native.openPinnedEntry(name)
	if err != nil {
		return nil, linuxError(err)
	}
	return &linuxEntryRef{native: pinned}, nil
}

func (directory *linuxDirectory) EntryMatches(name string, expected outputcap.CurrentEntryReference) (bool, error) {
	pinned, ok := expected.(*linuxEntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux pinned entry"))
	}
	matches, err := directory.native.pinnedEntryMatches(name, pinned.native)
	return matches, linuxError(err)
}

func (directory *linuxDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference, private bool,
) (outputcap.Directory, error) {
	pinned, ok := expected.(*linuxEntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux pinned directory"))
	}
	opened, err := directory.native.openPinnedDirectory(pinned.native, private)
	if err != nil {
		return nil, linuxError(err)
	}
	return &linuxDirectory{native: opened}, nil
}

func (directory *linuxDirectory) RemoveEntry(name string, expected outputcap.CurrentEntryReference) error {
	pinned, ok := expected.(*linuxEntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux pinned entry removal"))
	}
	return linuxError(directory.native.removePinnedEntry(name, pinned.native))
}

func (directory *linuxDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	right, ok := other.(*linuxDirectory)
	if !ok || directory == nil || directory.native == nil || right == nil || right.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux directory authority"))
	}
	same, err := linuxSameOpenDirectory(directory.native, right.native)
	return same, linuxError(err)
}

func (directory *linuxDirectory) SetModifiedTime(modified catalog.ModifiedTime) error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	return linuxError(directory.native.setModifiedTime(modified))
}

func (directory *linuxDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	var opened *linuxOutputDirectory
	var err error
	if private {
		opened, err = directory.native.openDirectoryExact(name, linuxOutputDirectoryMode)
	} else {
		opened, err = directory.native.openDirectory(name)
	}
	if err != nil {
		return nil, linuxError(err)
	}
	return directory.bindDirectoryOrigin(opened, name)
}

func (directory *linuxDirectory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	var created *linuxOutputDirectory
	var err error
	if private {
		created, err = directory.native.createPrivateDirectoryExact(name, linuxOutputDirectoryMode)
	} else {
		created, err = directory.native.createDirectoryExact(name, linuxPublicDirectoryCreateMode)
	}
	if err != nil {
		return nil, linuxError(err)
	}
	return directory.bindDirectoryOrigin(created, name)
}

func (directory *linuxDirectory) bindDirectoryOrigin(
	child *linuxOutputDirectory,
	name string,
) (outputcap.Directory, error) {
	parent, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxError(errors.Join(err, child.close()))
	}
	return &linuxDirectory{
		native: child,
		origin: &linuxDirectoryOrigin{parent: parent, name: name},
	}, nil
}

func (directory *linuxDirectory) ReservePublicDirectoryNoReplace(
	name string,
) (outputcap.Directory, outputcap.PublishNoReplaceOutcome, error) {
	if directory == nil || directory.native == nil {
		return nil, 0, errors.Join(
			outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	created, outcome, nativeErr := directory.native.reservePublicDirectoryNoReplace(
		name, linuxPublicDirectoryCreateMode)
	if created == nil {
		return nil, outcome, linuxError(nativeErr)
	}
	bound, bindErr := directory.bindDirectoryOrigin(created, name)
	if bindErr != nil {
		return nil, outputcap.PublishNoReplaceIndeterminate,
			errors.Join(linuxError(nativeErr), bindErr)
	}
	return bound, outcome, linuxError(nativeErr)
}

func (directory *linuxDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	source, ok := candidate.(*linuxDirectory)
	if !ok || directory == nil || directory.native == nil || source == nil || source.native == nil ||
		source.origin == nil || source.origin.parent == nil {
		return nil, errors.Join(
			outputcap.ErrFixedLinkSourceChanged, outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Linux installation candidate has no fixed origin"))
	}
	if err := source.origin.parent.renameDirectory(
		source.origin.name, source.native, directory.native, name, linuxRenameNoReplace,
	); err != nil {
		return nil, linuxError(err)
	}
	return directory.OpenDirectory(name, true)
}

func (directory *linuxDirectory) RemoveDirectory(name string, expected outputcap.Directory) error {
	target, ok := expected.(*linuxDirectory)
	if !ok || directory == nil || directory.native == nil || target == nil || target.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux directory removal authority"))
	}
	return linuxError(directory.native.unlinkDirectory(name, target.native))
}

func (directory *linuxDirectory) CreateFile(name string, private bool, size int64) (outputcap.MutableFile, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	permissions := uint32(linuxPublicFileCreateMode)
	if private {
		permissions = linuxOutputStateFileMode
	}
	// Reserve the origin before creation: losing it after the file is installed
	// would discard the only handle that proves cleanup ownership.
	parent, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxError(err)
	}
	created, err := parent.createRegularFileExactWithAuthority(name, permissions, size, private)
	if err != nil {
		return nil, linuxError(errors.Join(err, parent.close()))
	}
	return &linuxMutableFile{state: &linuxFileState{
		native: created, private: private,
		origin: &linuxFileOrigin{parent: parent, name: name},
	}}, nil
}

func (directory *linuxDirectory) OpenObservedFile(name string, private bool) (outputcap.ObservedFile, error) {
	state, err := directory.openFileState(name, private, linuxOutputFileObserved)
	if err != nil {
		return nil, err
	}
	return &linuxObservedFile{state: state}, nil
}

func (directory *linuxDirectory) OpenRecoveryDurabilityFile(
	name string,
	private bool,
) (outputcap.RecoveryDurabilityFile, error) {
	state, err := directory.openFileState(name, private, linuxOutputFileRecoveryDurability)
	if err != nil {
		return nil, err
	}
	return &linuxRecoveryDurabilityFile{state: state}, nil
}

func (directory *linuxDirectory) OpenMutableFile(name string, private bool) (outputcap.MutableFile, error) {
	state, err := directory.openFileState(name, private, linuxOutputFileMutable)
	if err != nil {
		return nil, err
	}
	return &linuxMutableFile{state: state}, nil
}

func (directory *linuxDirectory) openFileState(
	name string,
	private bool,
	access linuxOutputFileAccess,
) (*linuxFileState, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	var opened *linuxOutputRegularFile
	var err error
	if private {
		opened, err = directory.native.openRegularFileExact(name, access, linuxOutputStateFileMode)
	} else {
		opened, err = directory.native.openRegularFile(name, access)
	}
	if err != nil {
		return nil, linuxError(err)
	}
	return directory.bindFileState(opened, name, private)
}

func (directory *linuxDirectory) PublishFileNoReplace(
	source outputcap.FileIdentity,
	name string,
) (outputcap.PublishNoReplaceOutcome, error) {
	file, ok := linuxFileStateFrom(source)
	if !ok || directory == nil || directory.native == nil || file.native == nil {
		return 0, errors.Join(
			outputcap.ErrFixedLinkSourceChanged, outputcap.ErrUnsafeNamespace,
			errors.New("osfs: incompatible Linux file link authority"))
	}
	sourceParent := (*linuxOutputDirectory)(nil)
	sourceName := ""
	if file.origin != nil {
		sourceParent = file.origin.parent
		sourceName = file.origin.name
	}
	err := directory.native.linkRegularFileNoReplace(
		sourceParent, sourceName, file.native, name,
	)
	switch {
	case err == nil:
		return outputcap.PublishNoReplaceCommitted, nil
	case errors.Is(err, errLinuxOutputCollision):
		return outputcap.PublishNoReplaceCollision, nil
	case errors.Is(err, errLinuxOutputPublishIndeterminate):
		return outputcap.PublishNoReplaceIndeterminate, linuxError(err)
	default:
		return 0, linuxError(err)
	}
}

func (directory *linuxDirectory) LinkFileNoReplace(
	source outputcap.FileIdentity,
	name string,
) (outputcap.ObservedFile, error) {
	outcome, err := directory.PublishFileNoReplace(source, name)
	if err != nil {
		return nil, err
	}
	if outcome == outputcap.PublishNoReplaceCollision {
		return nil, outputcap.ErrNamespaceCollision
	}
	if outcome != outputcap.PublishNoReplaceCommitted {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Linux file publication outcome is indeterminate"))
	}
	linked, err := directory.native.openRegularFile(name, linuxOutputFileObserved)
	if err != nil {
		return nil, linuxError(err)
	}
	state, err := directory.bindFileState(linked, name, false)
	if err != nil {
		return nil, err
	}
	return &linuxObservedFile{state: state}, nil
}

func (directory *linuxDirectory) bindFileState(
	file *linuxOutputRegularFile,
	name string,
	private bool,
) (*linuxFileState, error) {
	parent, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxError(errors.Join(err, file.close()))
	}
	return &linuxFileState{
		native: file, private: private,
		origin: &linuxFileOrigin{parent: parent, name: name},
	}, nil
}

func (directory *linuxDirectory) CreateOrdinaryOutputStage(
	proofDirectory outputcap.Directory,
	name string,
	exactSize uint64,
) error {
	proof, ok := proofDirectory.(*linuxDirectory)
	if !ok || directory == nil || directory.native == nil || proof == nil || proof.native == nil ||
		name == "" || exactSize > math.MaxInt64 {
		return errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: invalid Linux ordinary-output stage authority"))
	}
	created, err := directory.native.createPublicProfileStage(proof.native, name, int64(exactSize))
	if created != nil {
		err = errors.Join(err, created.close())
	}
	return linuxError(err)
}

// CreateProcessStage retains the only deletion authority for its private name.
// Public metadata originates from the final parent through O_TMPFILE; keeping
// the inode handle across installation failures avoids guessing at owned files.
func (directory *linuxDirectory) CreateProcessStage(
	stageDirectory outputcap.Directory,
	name string,
	size int64,
) (outputcap.MutableFile, error) {
	stage, ok := stageDirectory.(*linuxDirectory)
	if !ok || directory == nil || directory.native == nil || stage == nil || stage.native == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	parent, err := stage.native.Duplicate()
	if err != nil {
		return nil, linuxError(err)
	}
	created, createErr := directory.native.createPublicProfileStage(stage.native, name, size)
	if created == nil {
		return nil, linuxError(errors.Join(createErr, parent.close()))
	}
	file := &linuxMutableFile{state: &linuxFileState{
		native: created,
		origin: &linuxFileOrigin{parent: parent, name: name},
	}}
	return file, linuxError(createErr)
}

func (directory *linuxDirectory) CreateLiveCleanupStage(
	proofDirectory outputcap.Directory,
	ticket checkpointmodel.LiveCleanupTicket,
) error {
	proof, ok := proofDirectory.(*linuxDirectory)
	if !ok || directory == nil || directory.native == nil || proof == nil || proof.native == nil ||
		!ticket.Valid() || ticket.Profile() != checkpointmodel.LiveCleanupLinuxExt4V1 ||
		ticket.State() != checkpointmodel.LiveCleanupTicketCommitted ||
		ticket.ExactSize() > math.MaxInt64 {
		return errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: invalid Linux live-cleanup stage authority"))
	}
	created, err := directory.native.createPublicProfileStage(
		proof.native, ticket.StageName(), int64(ticket.ExactSize()))
	if created != nil {
		err = errors.Join(err, created.close())
	}
	return linuxError(err)
}

func (directory *linuxDirectory) RemoveLiveCleanupStage(
	ticket checkpointmodel.LiveCleanupTicket,
	expected outputcap.FileIdentity,
) error {
	file, ok := linuxFileStateFrom(expected)
	if !ok || directory == nil || directory.native == nil || file.native == nil ||
		!ticket.Valid() || ticket.Profile() != checkpointmodel.LiveCleanupLinuxExt4V1 ||
		!directory.native.requireExactPermissions {
		return errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: invalid Linux live-cleanup removal authority"))
	}
	if err := directory.native.validatePrivateAuthority("remove Linux live-cleanup stage"); err != nil {
		return linuxError(err)
	}
	size, err := file.native.currentIdentity()
	if err != nil {
		return linuxError(err)
	}
	if size.size != ticket.ExactSize() ||
		(ticket.State() != checkpointmodel.LiveCleanupTicketCommitted &&
			ticket.State() != checkpointmodel.LiveCleanupStageCreated) {
		return errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Linux live-cleanup stage facts do not match its ticket"))
	}
	return linuxError(directory.native.unlinkRegularFile(ticket.StageName(), file.native))
}

func (directory *linuxDirectory) ReplacePrivateFile(source outputcap.FileIdentity, name string) error {
	file, ok := linuxFileStateFrom(source)
	if !ok || directory == nil || directory.native == nil || file.native == nil ||
		!file.private || file.origin == nil || file.origin.parent == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux state replacement source has no private fixed origin"))
	}
	return linuxError(file.origin.parent.renameRegularFile(
		file.origin.name, file.native, directory.native, name, linuxRenameReplace,
	))
}

func (directory *linuxDirectory) RemoveFile(name string, expected outputcap.FileIdentity) error {
	file, ok := linuxFileStateFrom(expected)
	if !ok || directory == nil || directory.native == nil || file.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux file removal authority"))
	}
	return linuxError(directory.native.unlinkRegularFile(name, file.native))
}

func (directory *linuxDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if directory == nil || directory.native == nil {
		return nil, false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	if existingOnly {
		lock, err := directory.native.acquireExistingStableLock(name)
		if err != nil {
			return nil, false, linuxError(err)
		}
		return newLinuxLock(lock), false, nil
	}
	file, err := directory.native.createPrivateRegularFileExact(name, linuxOutputStateFileMode, 0)
	created := err == nil
	if errors.Is(err, errLinuxOutputCollision) {
		lock, existingErr := directory.native.acquireExistingStableLock(name)
		if existingErr != nil {
			return nil, false, linuxError(existingErr)
		}
		return newLinuxLock(lock), false, nil
	}
	if err != nil {
		return nil, false, linuxError(err)
	}
	lock, err := linuxLockStableFile(file)
	if err != nil {
		return nil, false, linuxError(errors.Join(err, file.close()))
	}
	matches, err := directory.native.regularEntryMatches(name, file)
	if err != nil || !matches {
		return nil, false, linuxError(errors.Join(
			linuxUnsafe("lock stable output authority", "lock name differs from the locked object", nil),
			err,
			lock.Close(),
		))
	}
	return newLinuxLock(lock), created, nil
}
