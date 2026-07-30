//go:build windows

package outputwindows

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"strings"
	"unsafe"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

func (directory *windowsV3Directory) prepareIdentityClaim() ([]byte, error) {
	if _, err := directory.createOrGetPersistentObjectID(
		"prepare public output directory identity",
		directory.verifyPublicIdentityAuthority,
	); err != nil {
		return nil, err
	}
	return directory.identityClaim()
}

func (directory *windowsV3Directory) identityClaim() ([]byte, error) {
	return directory.encodeIdentityClaim(
		"claim output directory identity",
		directory.verifyPublicIdentityAuthority,
	)
}

func (directory *windowsV3Directory) preparePrivateIdentityClaim() ([]byte, error) {
	if _, err := directory.preparePrivatePersistentObjectID(); err != nil {
		return nil, err
	}
	return directory.privateIdentityClaim()
}

func (directory *windowsV3Directory) privateIdentityClaim() ([]byte, error) {
	return directory.encodeIdentityClaim(
		"claim private output directory identity",
		func() error { return directory.verify(true) },
	)
}

func (directory *windowsV3Directory) encodeIdentityClaim(
	operation string,
	authorize func() error,
) ([]byte, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if err := authorize(); err != nil {
		return nil, windowsV3Failure(
			operation, directory.path, windowsV3AuthorityFailureClass(err), err,
		)
	}
	facts, err := directory.inspector.Inspect(directory.handle())
	if err != nil {
		return nil, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe, err)
	}
	if err := windowsV3ValidateOpenedObject(facts, directory.volume, true); err != nil {
		return nil, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe, err)
	}
	if directory.objectIDState == nil {
		return nil, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe,
			errors.New("persistent NTFS object identity state is absent"))
	}
	objectID, prepared := directory.objectIDState.current()
	if !prepared {
		return nil, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe,
			errors.New("persistent NTFS object identity was not prepared"))
	}
	guid := strings.ToLower(facts.object.volume.guid)
	if len(guid) == 0 || len(guid) > windowsV3VolumeGUIDClaimMaxBytes {
		return nil, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe,
			errors.New("NTFS volume GUID identity exceeds the bounded claim format"))
	}
	claim := make([]byte, len(windowsV3DirectoryClaimTag)+4+len(guid)+8+len(objectID))
	copy(claim, windowsV3DirectoryClaimTag)
	offset := len(windowsV3DirectoryClaimTag)
	binary.BigEndian.PutUint32(claim[offset:], uint32(len(guid)))
	offset += 4
	copy(claim[offset:], guid)
	offset += len(guid)
	binary.BigEndian.PutUint64(claim[offset:], facts.object.volume.serial)
	offset += 8
	copy(claim[offset:], objectID[:])
	return claim, nil
}

type windowsV3PinnedEntry struct {
	handle     windows.Handle
	identity   windowsV3ObjectIdentity
	attributes uint32
	kind       outputcap.EntryKind
	name       string
}

func (directory *windowsV3Directory) openPinnedEntry(
	name string,
) (*windowsV3PinnedEntry, error) {
	return directory.openPinnedEntryForAccess(
		name, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
	)
}

func (directory *windowsV3Directory) openPinnedEntryForAccess(
	relative string,
	access uint32,
) (*windowsV3PinnedEntry, error) {
	const operation = "pin output entry"
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if err := directory.verify(false); err != nil {
		return nil, err
	}
	name, err := windowsV3RelativePath(relative, true)
	if err != nil {
		return nil, windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe, err)
	}
	handle, status, err := windowsV3OpenNativeWithOptions(
		directory.handle(), name, access,
		windows.FILE_OPEN, 0, 0, nil,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.OBJ_CASE_INSENSITIVE,
	)
	if err != nil {
		return nil, windowsV3NativeOperationFailure(operation, name, err)
	}
	closeFailure := func(cause error) (*windowsV3PinnedEntry, error) {
		return nil, errors.Join(
			windowsV3Failure(operation, name, errWindowsV3OutputUnsafe, cause),
			windows.CloseHandle(handle),
		)
	}
	if _, err := windowsV3CreationStatus(windows.FILE_OPEN, status); err != nil {
		return closeFailure(err)
	}
	if err := windowsV3VerifyOpenedExactName(handle, name); err != nil {
		return closeFailure(err)
	}
	identity, attributes, kind, err := windowsV3ReadPinnedEntryIdentity(handle, directory.volume)
	if err != nil {
		return closeFailure(err)
	}
	if err := directory.verify(false); err != nil {
		return closeFailure(err)
	}
	return &windowsV3PinnedEntry{
		handle: handle, identity: identity, attributes: attributes, kind: kind, name: name,
	}, nil
}

func windowsV3ReadPinnedEntryIdentity(
	handle windows.Handle,
	expectedVolume windowsV3VolumeIdentity,
) (windowsV3ObjectIdentity, uint32, outputcap.EntryKind, error) {
	var fileID windowsV3FileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&fileID)), uint32(unsafe.Sizeof(fileID)),
	); err != nil {
		return windowsV3ObjectIdentity{}, 0, outputcap.EntryAbsent, err
	}
	identity := windowsV3ObjectIdentity{volume: expectedVolume, fileID: fileID.FileID}
	if fileID.VolumeSerialNumber != expectedVolume.serial || !identity.valid() {
		return windowsV3ObjectIdentity{}, 0, outputcap.EntryAbsent,
			errors.New("entry is outside the fixed NTFS volume identity")
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsV3ObjectIdentity{}, 0, outputcap.EntryAbsent, err
	}
	kind := outputcap.EntryRegularFile
	if information.FileAttributes&windowsV3CloudAttributeMask != 0 {
		kind = outputcap.EntryOther
	} else if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		kind = outputcap.EntryDirectory
	}
	return identity, information.FileAttributes, kind, nil
}

func (entry *windowsV3PinnedEntry) close() error {
	if entry == nil || entry.handle == windows.InvalidHandle || entry.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(entry.handle)
	entry.handle = windows.InvalidHandle
	return err
}

func (entry *windowsV3PinnedEntry) validate() error {
	const operation = "validate pinned output entry"
	if entry == nil || entry.handle == windows.InvalidHandle || entry.handle == 0 || !entry.identity.valid() {
		return windowsV3Failure(operation, "", errWindowsV3OutputUnsafe,
			errors.New("pinned entry handle is closed or incomplete"))
	}
	identity, attributes, kind, err := windowsV3ReadPinnedEntryIdentity(entry.handle, entry.identity.volume)
	if err != nil {
		return windowsV3Failure(operation, entry.name, errWindowsV3OutputUnsafe, err)
	}
	if !entry.identity.same(identity) || entry.kind != kind ||
		entry.attributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windowsV3CloudAttributeMask) !=
			attributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windowsV3CloudAttributeMask) {
		return windowsV3Failure(operation, entry.name, errWindowsV3OutputUnsafe,
			errors.New("pinned entry identity or type changed"))
	}
	return nil
}

func (entry *windowsV3PinnedEntry) allocatedSize() (uint64, error) {
	const operation = "inspect pinned output entry allocation"
	if err := entry.validate(); err != nil {
		return 0, err
	}
	var information windowsV3FileStandardInformation
	if err := windows.GetFileInformationByHandleEx(
		entry.handle, windows.FileStandardInfo, (*byte)(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)),
	); err != nil {
		return 0, windowsV3Failure(operation, entry.name, errWindowsV3OutputUnsafe, err)
	}
	if information.AllocationSize < 0 {
		return 0, windowsV3Failure(operation, entry.name, errWindowsV3OutputUnsafe,
			errors.New("entry allocation metadata is invalid"))
	}
	return uint64(information.AllocationSize), nil
}

func (directory *windowsV3Directory) pinnedEntryMatches(
	name string,
	expected *windowsV3PinnedEntry,
) (bool, error) {
	const operation = "compare pinned output entry"
	if expected == nil || expected.handle == windows.InvalidHandle || expected.handle == 0 ||
		expected.identity.volume != directory.volume {
		return false, windowsV3Failure(operation, name, errWindowsV3OutputUnsafe,
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

func (directory *windowsV3Directory) openPinnedDirectory(
	expected *windowsV3PinnedEntry,
	private bool,
) (*windowsV3Directory, error) {
	const operation = "open pinned output directory"
	if expected == nil || expected.kind != outputcap.EntryDirectory ||
		expected.handle == windows.InvalidHandle || expected.handle == 0 ||
		expected.identity.volume != directory.volume {
		return nil, windowsV3Failure(operation, "", errWindowsV3OutputUnsafe,
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
			windowsV3Failure(operation, expected.name, errWindowsV3OutputUnsafe,
				errors.New("opened directory differs from the pinned entry")),
			err, opened.Close(),
		)
	}
	return opened, nil
}

func (directory *windowsV3Directory) removePinnedEntry(
	name string,
	expected *windowsV3PinnedEntry,
) (resultErr error) {
	const operation = "remove pinned output entry"
	if expected == nil || expected.handle == windows.InvalidHandle || expected.handle == 0 ||
		expected.identity.volume != directory.volume {
		return windowsV3Failure(operation, name, errWindowsV3OutputUnsafe,
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
		return windowsV3Failure(operation, name, errWindowsV3OutputUnsafe,
			errors.New("current name differs from the pinned entry"))
	}
	if err := windowsV3RemoveHandle(current.handle); err != nil {
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

func (file *windowsV3File) Size() (uint64, error) {
	metadata, err := windowsV3ReadHandleMetadata(file.handle())
	if err != nil {
		return 0, windowsV3Failure("inspect output file size", file.path, errWindowsV3OutputUnsafe, err)
	}
	return metadata.size, nil
}

type windowsV3FileStandardInformation struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  uint8
	Directory      uint8
	_              [2]byte
}

func (file *windowsV3File) allocatedSize() (uint64, error) {
	const operation = "inspect output file allocation"
	if err := file.verify(false); err != nil {
		return 0, err
	}
	var information windowsV3FileStandardInformation
	if err := windows.GetFileInformationByHandleEx(
		file.handle(), windows.FileStandardInfo, (*byte)(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)),
	); err != nil {
		return 0, windowsV3Failure(operation, file.path, errWindowsV3OutputUnsafe, err)
	}
	if information.AllocationSize < 0 || information.Directory != 0 {
		return 0, windowsV3Failure(operation, file.path, errWindowsV3OutputUnsafe,
			errors.New("file allocation metadata is invalid"))
	}
	return uint64(information.AllocationSize), nil
}

type windowsV3OutputMetadata struct {
	size          uint64
	modifiedTicks uint64
}

func (directory *windowsV3Directory) Duplicate() (*windowsV3Directory, error) {
	const operation = "duplicate output directory authority"
	if err := directory.usable(); err != nil {
		return nil, err
	}
	var duplicate windows.Handle
	process := windows.CurrentProcess()
	if err := windows.DuplicateHandle(
		process, directory.handle(), process, &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return nil, windowsV3NativeOperationFailure(operation, directory.path, err)
	}
	wrapped := os.NewFile(uintptr(duplicate), directory.path)
	if wrapped == nil {
		_ = windows.CloseHandle(duplicate)
		return nil, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe,
			errors.New("wrap duplicated directory handle"))
	}
	result := &windowsV3Directory{
		file: wrapped, path: directory.path, volume: directory.volume,
		objectIDs: directory.objectIDs, objectIDState: directory.objectIDState,
		inspector: directory.inspector, policy: directory.policy,
		ancestryAuthority: directory.ancestryAuthority, enumerate: directory.enumerate,
		createObserver: directory.createObserver, private: directory.private,
		placementGuard: directory.placementGuard, selfPlacementGuard: directory.selfPlacementGuard,
	}
	if err := result.verify(false); err != nil {
		return nil, errors.Join(err, result.Close())
	}
	same, err := sameWindowsV3OpenedDirectory(directory, result)
	if err != nil || !same {
		return nil, errors.Join(windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe,
			errors.New("duplicated handle does not identify the fixed directory")), err, result.Close())
	}
	return result, nil
}
