//go:build windows

package outputwindows

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"strings"
	"unsafe"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

func (directory *windowsDirectory) prepareIdentityClaim() ([]byte, error) {
	if _, err := directory.createOrGetPersistentObjectID(
		"prepare public output directory identity",
		directory.verifyPublicIdentityAuthority,
	); err != nil {
		return nil, err
	}
	return directory.identityClaim()
}

func (directory *windowsDirectory) identityClaim() ([]byte, error) {
	return directory.encodeIdentityClaim(
		"claim output directory identity",
		directory.verifyPublicIdentityAuthority,
	)
}

func (directory *windowsDirectory) preparePrivateIdentityClaim() ([]byte, error) {
	if _, err := directory.preparePrivatePersistentObjectID(); err != nil {
		return nil, err
	}
	return directory.privateIdentityClaim()
}

func (directory *windowsDirectory) privateIdentityClaim() ([]byte, error) {
	return directory.encodeIdentityClaim(
		"claim private output directory identity",
		func() error { return directory.verify(true) },
	)
}

func (directory *windowsDirectory) encodeIdentityClaim(
	operation string,
	authorize func() error,
) ([]byte, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if err := authorize(); err != nil {
		return nil, windowsFailure(
			operation, directory.path, windowsAuthorityFailureClass(err), err,
		)
	}
	facts, err := directory.inspector.Inspect(directory.handle())
	if err != nil {
		return nil, windowsFailure(operation, directory.path, errWindowsOutputUnsafe, err)
	}
	if err := windowsValidateOpenedObject(facts, directory.volume, true); err != nil {
		return nil, windowsFailure(operation, directory.path, errWindowsOutputUnsafe, err)
	}
	if directory.objectIDState == nil {
		return nil, windowsFailure(operation, directory.path, errWindowsOutputUnsafe,
			errors.New("persistent NTFS object identity state is absent"))
	}
	objectID, prepared := directory.objectIDState.current()
	if !prepared {
		return nil, windowsFailure(operation, directory.path, errWindowsOutputUnsafe,
			errors.New("persistent NTFS object identity was not prepared"))
	}
	guid := strings.ToLower(facts.object.volume.guid)
	if len(guid) == 0 || len(guid) > windowsVolumeGUIDClaimMaxBytes {
		return nil, windowsFailure(operation, directory.path, errWindowsOutputUnsafe,
			errors.New("NTFS volume GUID identity exceeds the bounded claim format"))
	}
	claim := make([]byte, len(windowsDirectoryClaimTag)+4+len(guid)+8+len(objectID))
	copy(claim, windowsDirectoryClaimTag)
	offset := len(windowsDirectoryClaimTag)
	binary.BigEndian.PutUint32(claim[offset:], uint32(len(guid)))
	offset += 4
	copy(claim[offset:], guid)
	offset += len(guid)
	binary.BigEndian.PutUint64(claim[offset:], facts.object.volume.serial)
	offset += 8
	copy(claim[offset:], objectID[:])
	return claim, nil
}

type windowsPinnedEntry struct {
	handle     windows.Handle
	identity   windowsObjectIdentity
	attributes uint32
	kind       outputcap.EntryKind
	name       string
}

func (directory *windowsDirectory) openPinnedEntry(
	name string,
) (*windowsPinnedEntry, error) {
	return directory.openPinnedEntryForAccess(
		name, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
	)
}

func (directory *windowsDirectory) openPinnedEntryForAccess(
	relative string,
	access uint32,
) (*windowsPinnedEntry, error) {
	const operation = "pin output entry"
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if err := directory.verify(false); err != nil {
		return nil, err
	}
	name, err := windowsRelativePath(relative, true)
	if err != nil {
		return nil, windowsFailure(operation, relative, errWindowsOutputUnsafe, err)
	}
	handle, status, err := windowsOpenNativeWithOptions(
		directory.handle(), name, access,
		windows.FILE_OPEN, 0, 0, nil,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.OBJ_CASE_INSENSITIVE,
	)
	if err != nil {
		return nil, windowsNativeOperationFailure(operation, name, err)
	}
	closeFailure := func(cause error) (*windowsPinnedEntry, error) {
		return nil, errors.Join(
			windowsFailure(operation, name, errWindowsOutputUnsafe, cause),
			windows.CloseHandle(handle),
		)
	}
	if _, err := windowsCreationStatus(windows.FILE_OPEN, status); err != nil {
		return closeFailure(err)
	}
	if err := windowsVerifyOpenedExactName(handle, name); err != nil {
		return closeFailure(err)
	}
	identity, attributes, kind, err := windowsReadPinnedEntryIdentity(handle, directory.volume)
	if err != nil {
		return closeFailure(err)
	}
	if err := directory.verify(false); err != nil {
		return closeFailure(err)
	}
	return &windowsPinnedEntry{
		handle: handle, identity: identity, attributes: attributes, kind: kind, name: name,
	}, nil
}

func windowsReadPinnedEntryIdentity(
	handle windows.Handle,
	expectedVolume windowsVolumeIdentity,
) (windowsObjectIdentity, uint32, outputcap.EntryKind, error) {
	var fileID windowsFileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&fileID)), uint32(unsafe.Sizeof(fileID)),
	); err != nil {
		return windowsObjectIdentity{}, 0, outputcap.EntryAbsent, err
	}
	identity := windowsObjectIdentity{volume: expectedVolume, fileID: fileID.FileID}
	if fileID.VolumeSerialNumber != expectedVolume.serial || !identity.valid() {
		return windowsObjectIdentity{}, 0, outputcap.EntryAbsent,
			errors.New("entry is outside the fixed NTFS volume identity")
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsObjectIdentity{}, 0, outputcap.EntryAbsent, err
	}
	kind := outputcap.EntryRegularFile
	if information.FileAttributes&windowsCloudAttributeMask != 0 {
		kind = outputcap.EntryOther
	} else if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		kind = outputcap.EntryDirectory
	}
	return identity, information.FileAttributes, kind, nil
}

func (entry *windowsPinnedEntry) close() error {
	if entry == nil || entry.handle == windows.InvalidHandle || entry.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(entry.handle)
	entry.handle = windows.InvalidHandle
	return err
}

func (entry *windowsPinnedEntry) validate() error {
	const operation = "validate pinned output entry"
	if entry == nil || entry.handle == windows.InvalidHandle || entry.handle == 0 || !entry.identity.valid() {
		return windowsFailure(operation, "", errWindowsOutputUnsafe,
			errors.New("pinned entry handle is closed or incomplete"))
	}
	identity, attributes, kind, err := windowsReadPinnedEntryIdentity(entry.handle, entry.identity.volume)
	if err != nil {
		return windowsFailure(operation, entry.name, errWindowsOutputUnsafe, err)
	}
	if !entry.identity.same(identity) || entry.kind != kind ||
		entry.attributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windowsCloudAttributeMask) !=
			attributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windowsCloudAttributeMask) {
		return windowsFailure(operation, entry.name, errWindowsOutputUnsafe,
			errors.New("pinned entry identity or type changed"))
	}
	return nil
}

func (directory *windowsDirectory) pinnedEntryMatches(
	name string,
	expected *windowsPinnedEntry,
) (bool, error) {
	const operation = "compare pinned output entry"
	if expected == nil || expected.handle == windows.InvalidHandle || expected.handle == 0 ||
		expected.identity.volume != directory.volume {
		return false, windowsFailure(operation, name, errWindowsOutputUnsafe,
			errors.New("pinned entry belongs to an incompatible authority"))
	}
	if err := expected.validate(); err != nil {
		return false, err
	}
	current, err := directory.openPinnedEntry(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	same := expected.kind == current.kind && expected.identity.same(current.identity)
	return same, errors.Join(current.close())
}

func (directory *windowsDirectory) openPinnedDirectory(
	expected *windowsPinnedEntry,
	private bool,
) (*windowsDirectory, error) {
	const operation = "open pinned output directory"
	if expected == nil || expected.kind != outputcap.EntryDirectory ||
		expected.handle == windows.InvalidHandle || expected.handle == 0 ||
		expected.identity.volume != directory.volume {
		return nil, windowsFailure(operation, "", errWindowsOutputUnsafe,
			errors.New("pinned entry is not an open directory in this authority"))
	}
	if err := expected.validate(); err != nil {
		return nil, err
	}
	opened, err := directory.openDirectory(expected.name, private, windows.FILE_OPEN)
	if err != nil {
		return nil, err
	}
	facts, err := opened.inspector.Inspect(opened.handle())
	if err != nil || !expected.identity.same(facts.object) {
		return nil, errors.Join(
			windowsFailure(operation, expected.name, errWindowsOutputUnsafe,
				errors.New("opened directory differs from the pinned entry")),
			err, opened.Close(),
		)
	}
	return opened, nil
}

func (directory *windowsDirectory) removePinnedEntry(
	name string,
	expected *windowsPinnedEntry,
) (resultErr error) {
	const operation = "remove pinned output entry"
	if expected == nil || expected.handle == windows.InvalidHandle || expected.handle == 0 ||
		expected.identity.volume != directory.volume {
		return windowsFailure(operation, name, errWindowsOutputUnsafe,
			errors.New("pinned entry belongs to an incompatible authority"))
	}
	if err := expected.validate(); err != nil {
		return err
	}
	current, err := directory.openPinnedEntryForAccess(
		name, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
	)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, current.close()) }()
	if expected.kind != current.kind || !expected.identity.same(current.identity) {
		return windowsFailure(operation, name, errWindowsOutputUnsafe,
			errors.New("current name differs from the pinned entry"))
	}
	if err := windowsRemoveHandle(current.handle); err != nil {
		return err
	}
	return directory.Sync()
}

// NTFS name lookup is case-insensitive in this backend. Exact byte equality is
// included so duplicate kernel observations are rejected by the same check.
func stringsEqualFoldExact(left, right string) bool {
	return left == right || equalFoldWindowsName(left, right)
}

func equalFoldWindowsName(left, right string) bool {
	// strings.EqualFold follows Unicode simple folding, which is stricter than
	// accepting two names that this case-insensitive handle can resolve to one
	// entry. The admission layer still performs its fuller NTFS alias check.
	return strings.EqualFold(left, right)
}

func (file *windowsFile) Size() (uint64, error) {
	metadata, err := windowsReadHandleMetadata(file.handle())
	if err != nil {
		return 0, windowsFailure("inspect output file size", file.path, errWindowsOutputUnsafe, err)
	}
	return metadata.size, nil
}

type windowsOutputMetadata struct {
	size          uint64
	modifiedTicks uint64
}

func (directory *windowsDirectory) Duplicate() (*windowsDirectory, error) {
	const operation = "duplicate output directory authority"
	if err := directory.usable(); err != nil {
		return nil, err
	}
	var duplicate windows.Handle
	process := windows.CurrentProcess()
	if err := windows.DuplicateHandle(
		process, directory.handle(), process, &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return nil, windowsNativeOperationFailure(operation, directory.path, err)
	}
	wrapped := os.NewFile(uintptr(duplicate), directory.path)
	if wrapped == nil {
		_ = windows.CloseHandle(duplicate)
		return nil, windowsFailure(operation, directory.path, errWindowsOutputUnsafe,
			errors.New("wrap duplicated directory handle"))
	}
	result := &windowsDirectory{
		file: wrapped, metadataHandle: windows.InvalidHandle, metadataOpenErr: directory.metadataOpenErr,
		path: directory.path, volume: directory.volume,
		objectIDs: directory.objectIDs, objectIDState: directory.objectIDState,
		inspector: directory.inspector, policy: directory.policy,
		ancestryAuthority: directory.ancestryAuthority, enumerate: directory.enumerate,
		createObserver: directory.createObserver, private: directory.private,
		placementGuard: directory.placementGuard, selfPlacementGuard: directory.selfPlacementGuard,
	}
	if directory.metadataHandleOpen {
		if err := windows.DuplicateHandle(
			process, directory.metadataHandle, process, &result.metadataHandle, 0, false, windows.DUPLICATE_SAME_ACCESS,
		); err != nil {
			return nil, errors.Join(
				windowsNativeOperationFailure(operation, directory.path, err), result.Close(),
			)
		}
		result.metadataHandleOpen = true
	}
	if err := result.verify(false); err != nil {
		return nil, errors.Join(err, result.Close())
	}
	same, err := sameWindowsOpenedDirectory(directory, result)
	if err != nil || !same {
		return nil, errors.Join(windowsFailure(operation, directory.path, errWindowsOutputUnsafe,
			errors.New("duplicated handle does not identify the fixed directory")), err, result.Close())
	}
	return result, nil
}
