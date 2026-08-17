//go:build linux

package outputlinux

import (
	"errors"
	"math"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func (directory *linuxV3Directory) PersistentDirectoryIdentityClaim() ([]byte, error) {
	if directory == nil || directory.native == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	claim, err := directory.native.identityClaim()
	return claim, linuxV3Error(err)
}

func (directory *linuxV3Directory) PreparePersistentDirectoryIdentityClaim() ([]byte, error) {
	if directory == nil || directory.native == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	claim, err := directory.native.prepareIdentityClaim()
	return claim, linuxV3Error(err)
}

func (directory *linuxV3Directory) Close() error {
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
	return linuxV3Error(errors.Join(originErr, nativeErr))
}

func (directory *linuxV3Directory) Duplicate() (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	native, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxV3Error(err)
	}
	result := &linuxV3Directory{native: native}
	if directory.origin != nil {
		parent, duplicateErr := directory.origin.parent.Duplicate()
		if duplicateErr != nil {
			return nil, linuxV3Error(errors.Join(duplicateErr, native.close()))
		}
		result.origin = &linuxV3DirectoryOrigin{parent: parent, name: directory.origin.name}
	}
	return result, nil
}

func (directory *linuxV3Directory) Sync() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	return linuxV3Error(directory.native.sync())
}

func (directory *linuxV3Directory) Names(limit int) ([]string, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	names, err := directory.native.names(limit)
	return names, linuxV3Error(err)
}

func (directory *linuxV3Directory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if directory == nil || directory.native == nil {
		return outputcap.EntryAbsent, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	kind, err := directory.native.observeEntry(name)
	return kind, linuxV3Error(err)
}

func (directory *linuxV3Directory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	if directory == nil || directory.native == nil {
		return outputcap.EntryAbsent, false, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Linux directory authority is closed"),
		)
	}
	kind, exact, err := directory.native.classifyExactEntry(name)
	return kind, exact, linuxV3Error(err)
}

func (directory *linuxV3Directory) validatePublicEntryName(name string) error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	_, exact, err := directory.native.classifyExactEntry(name)
	if err != nil {
		return linuxV3Error(err)
	}
	if !exact {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}

func (directory *linuxV3Directory) ValidatePublicEntryNames(names []string) error {
	for _, name := range names {
		if err := directory.validatePublicEntryName(name); err != nil {
			return err
		}
	}
	return nil
}

func (directory *linuxV3Directory) ValidateCreateAuthority() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	return linuxV3Error(directory.native.validateCreateAuthority())
}

func (directory *linuxV3Directory) ValidateMetadataAuthority() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	if err := directory.native.validateMetadataAuthority(); err != nil {
		return linuxV3Error(err)
	}
	return nil
}

func (directory *linuxV3Directory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	pinned, err := directory.native.openPinnedEntry(name)
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return &linuxV3EntryRef{native: pinned}, nil
}

func (directory *linuxV3Directory) EntryMatches(name string, expected outputcap.CurrentEntryReference) (bool, error) {
	pinned, ok := expected.(*linuxV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux pinned entry"))
	}
	matches, err := directory.native.pinnedEntryMatches(name, pinned.native)
	return matches, linuxV3Error(err)
}

func (directory *linuxV3Directory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference, private bool,
) (outputcap.Directory, error) {
	pinned, ok := expected.(*linuxV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux pinned directory"))
	}
	opened, err := directory.native.openPinnedDirectory(pinned.native, private)
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return &linuxV3Directory{native: opened}, nil
}

func (directory *linuxV3Directory) RemoveEntry(name string, expected outputcap.CurrentEntryReference) error {
	pinned, ok := expected.(*linuxV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux pinned entry removal"))
	}
	return linuxV3Error(directory.native.removePinnedEntry(name, pinned.native))
}

func (directory *linuxV3Directory) SameDirectory(other outputcap.Directory) (bool, error) {
	right, ok := other.(*linuxV3Directory)
	if !ok || directory == nil || directory.native == nil || right == nil || right.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux directory authority"))
	}
	same, err := linuxSameOpenDirectory(directory.native, right.native)
	return same, linuxV3Error(err)
}

func (directory *linuxV3Directory) SetModifiedTime(modified catalog.ModifiedTime) error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	return linuxV3Error(directory.native.setModifiedTime(modified))
}

func (directory *linuxV3Directory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
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
		return nil, linuxV3Error(err)
	}
	return directory.bindDirectoryOrigin(opened, name)
}

func (directory *linuxV3Directory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
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
		return nil, linuxV3Error(err)
	}
	return directory.bindDirectoryOrigin(created, name)
}

func (directory *linuxV3Directory) bindDirectoryOrigin(
	child *linuxOutputDirectory,
	name string,
) (outputcap.Directory, error) {
	parent, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxV3Error(errors.Join(err, child.close()))
	}
	return &linuxV3Directory{
		native: child,
		origin: &linuxV3DirectoryOrigin{parent: parent, name: name},
	}, nil
}

func (directory *linuxV3Directory) ReservePublicDirectoryNoReplace(
	name string,
) (outputcap.Directory, outputcap.PublishNoReplaceOutcome, error) {
	if directory == nil || directory.native == nil {
		return nil, 0, errors.Join(
			outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	created, outcome, nativeErr := directory.native.reservePublicDirectoryNoReplace(
		name, linuxPublicDirectoryCreateMode)
	if created == nil {
		return nil, outcome, linuxV3Error(nativeErr)
	}
	bound, bindErr := directory.bindDirectoryOrigin(created, name)
	if bindErr != nil {
		return nil, outputcap.PublishNoReplaceIndeterminate,
			errors.Join(linuxV3Error(nativeErr), bindErr)
	}
	return bound, outcome, linuxV3Error(nativeErr)
}

func (directory *linuxV3Directory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	source, ok := candidate.(*linuxV3Directory)
	if !ok || directory == nil || directory.native == nil || source == nil || source.native == nil ||
		source.origin == nil || source.origin.parent == nil {
		return nil, errors.Join(
			outputcap.ErrFixedLinkSourceChanged, outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Linux installation candidate has no fixed origin"))
	}
	if err := source.origin.parent.renameDirectory(
		source.origin.name, source.native, directory.native, name, linuxRenameNoReplace,
	); err != nil {
		return nil, linuxV3Error(err)
	}
	return directory.OpenDirectory(name, true)
}

func (directory *linuxV3Directory) RemoveDirectory(name string, expected outputcap.Directory) error {
	target, ok := expected.(*linuxV3Directory)
	if !ok || directory == nil || directory.native == nil || target == nil || target.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux directory removal authority"))
	}
	return linuxV3Error(directory.native.unlinkDirectory(name, target.native))
}

func (directory *linuxV3Directory) CreateFile(name string, private bool, size int64) (outputcap.MutableFile, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	permissions := uint32(linuxPublicFileCreateMode)
	if private {
		permissions = linuxOutputStateFileMode
	}
	created, err := directory.native.createRegularFileExactWithAuthority(name, permissions, size, private)
	if err != nil {
		return nil, linuxV3Error(err)
	}
	parent, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxV3Error(errors.Join(err, created.close()))
	}
	return &linuxV3MutableFile{state: &linuxV3FileState{
		native: created, private: private,
		origin: &linuxV3FileOrigin{parent: parent, name: name},
	}}, nil
}

func (directory *linuxV3Directory) OpenObservedFile(name string, private bool) (outputcap.ObservedFile, error) {
	state, err := directory.openFileState(name, private, linuxOutputFileObserved)
	if err != nil {
		return nil, err
	}
	return &linuxV3ObservedFile{state: state}, nil
}

func (directory *linuxV3Directory) OpenRecoveryDurabilityFile(
	name string,
	private bool,
) (outputcap.RecoveryDurabilityFile, error) {
	state, err := directory.openFileState(name, private, linuxOutputFileRecoveryDurability)
	if err != nil {
		return nil, err
	}
	return &linuxV3RecoveryDurabilityFile{state: state}, nil
}

func (directory *linuxV3Directory) OpenMutableFile(name string, private bool) (outputcap.MutableFile, error) {
	state, err := directory.openFileState(name, private, linuxOutputFileMutable)
	if err != nil {
		return nil, err
	}
	return &linuxV3MutableFile{state: state}, nil
}

func (directory *linuxV3Directory) openFileState(
	name string,
	private bool,
	access linuxOutputFileAccess,
) (*linuxV3FileState, error) {
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
		return nil, linuxV3Error(err)
	}
	return directory.bindFileState(opened, name, private)
}

func (directory *linuxV3Directory) PublishFileNoReplace(
	source outputcap.FileIdentity,
	name string,
) (outputcap.PublishNoReplaceOutcome, error) {
	file, ok := linuxV3FileStateFrom(source)
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
		return outputcap.PublishNoReplaceIndeterminate, linuxV3Error(err)
	default:
		return 0, linuxV3Error(err)
	}
}

func (directory *linuxV3Directory) LinkFileNoReplace(
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
		return nil, linuxV3Error(err)
	}
	state, err := directory.bindFileState(linked, name, false)
	if err != nil {
		return nil, err
	}
	return &linuxV3ObservedFile{state: state}, nil
}

func (directory *linuxV3Directory) bindFileState(
	file *linuxOutputRegularFile,
	name string,
	private bool,
) (*linuxV3FileState, error) {
	parent, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxV3Error(errors.Join(err, file.close()))
	}
	return &linuxV3FileState{
		native: file, private: private,
		origin: &linuxV3FileOrigin{parent: parent, name: name},
	}, nil
}

func (directory *linuxV3Directory) CreateOrdinaryOutputStage(
	proofDirectory outputcap.Directory,
	name string,
	exactSize uint64,
) error {
	proof, ok := proofDirectory.(*linuxV3Directory)
	if !ok || directory == nil || directory.native == nil || proof == nil || proof.native == nil ||
		name == "" || exactSize > math.MaxInt64 {
		return errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: invalid Linux ordinary-output stage authority"))
	}
	created, err := directory.native.createLiveCleanupStage(proof.native, name, int64(exactSize))
	if err != nil {
		return linuxV3Error(err)
	}
	return linuxV3Error(created.close())
}

func (directory *linuxV3Directory) CreateLiveCleanupStage(
	proofDirectory outputcap.Directory,
	ticket checkpointmodel.LiveCleanupTicket,
) error {
	proof, ok := proofDirectory.(*linuxV3Directory)
	if !ok || directory == nil || directory.native == nil || proof == nil || proof.native == nil ||
		!ticket.Valid() || ticket.Profile() != checkpointmodel.LiveCleanupLinuxExt4V1 ||
		ticket.State() != checkpointmodel.LiveCleanupTicketCommitted ||
		ticket.ExactSize() > math.MaxInt64 {
		return errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: invalid Linux live-cleanup stage authority"))
	}
	created, err := directory.native.createLiveCleanupStage(
		proof.native, ticket.StageName(), int64(ticket.ExactSize()))
	if err != nil {
		return linuxV3Error(err)
	}
	return linuxV3Error(created.close())
}

func (directory *linuxV3Directory) RemoveLiveCleanupStage(
	ticket checkpointmodel.LiveCleanupTicket,
	expected outputcap.FileIdentity,
) error {
	file, ok := linuxV3FileStateFrom(expected)
	if !ok || directory == nil || directory.native == nil || file.native == nil ||
		!ticket.Valid() || ticket.Profile() != checkpointmodel.LiveCleanupLinuxExt4V1 ||
		!directory.native.requireExactPermissions {
		return errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: invalid Linux live-cleanup removal authority"))
	}
	if err := directory.native.validatePrivateAuthority("remove Linux live-cleanup stage"); err != nil {
		return linuxV3Error(err)
	}
	size, err := file.native.currentIdentity()
	if err != nil {
		return linuxV3Error(err)
	}
	if size.size != ticket.ExactSize() ||
		(ticket.State() != checkpointmodel.LiveCleanupTicketCommitted &&
			ticket.State() != checkpointmodel.LiveCleanupStageCreated) {
		return errors.Join(outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Linux live-cleanup stage facts do not match its ticket"))
	}
	return linuxV3Error(directory.native.unlinkRegularFile(ticket.StageName(), file.native))
}

func (directory *linuxV3Directory) ReplacePrivateFile(source outputcap.FileIdentity, name string) error {
	file, ok := linuxV3FileStateFrom(source)
	if !ok || directory == nil || directory.native == nil || file.native == nil ||
		!file.private || file.origin == nil || file.origin.parent == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux state replacement source has no private fixed origin"))
	}
	return linuxV3Error(file.origin.parent.renameRegularFile(
		file.origin.name, file.native, directory.native, name, linuxRenameReplace,
	))
}

func (directory *linuxV3Directory) RemoveFile(name string, expected outputcap.FileIdentity) error {
	file, ok := linuxV3FileStateFrom(expected)
	if !ok || directory == nil || directory.native == nil || file.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux file removal authority"))
	}
	return linuxV3Error(directory.native.unlinkRegularFile(name, file.native))
}

func (directory *linuxV3Directory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if directory == nil || directory.native == nil {
		return nil, false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	if existingOnly {
		lock, err := directory.native.acquireExistingStableLock(name)
		if err != nil {
			return nil, false, linuxV3Error(err)
		}
		return newLinuxV3Lock(lock), false, nil
	}
	file, err := directory.native.createPrivateRegularFileExact(name, linuxOutputStateFileMode, 0)
	created := err == nil
	if errors.Is(err, errLinuxOutputCollision) {
		lock, existingErr := directory.native.acquireExistingStableLock(name)
		if existingErr != nil {
			return nil, false, linuxV3Error(existingErr)
		}
		return newLinuxV3Lock(lock), false, nil
	}
	if err != nil {
		return nil, false, linuxV3Error(err)
	}
	lock, err := linuxLockStableFile(file)
	if err != nil {
		return nil, false, linuxV3Error(errors.Join(err, file.close()))
	}
	matches, err := directory.native.regularEntryMatches(name, file)
	if err != nil || !matches {
		return nil, false, linuxV3Error(errors.Join(
			linuxUnsafe("lock stable output authority", "lock name differs from the locked object", nil),
			err,
			lock.Close(),
		))
	}
	return newLinuxV3Lock(lock), created, nil
}
