//go:build windows

package osfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode/utf16"
	"unsafe"

	"github.com/windshare/windshare/core/catalog"
	"golang.org/x/sys/windows"
)

const (
	windowsV3DirectoryReadBufferBytes = 32 << 10
	windowsV3DirectoryClaimTag        = "windows/ntfs/directory-object/v2"
	windowsV3VolumeGUIDClaimMaxBytes  = 128
	windowsV3DirectoryClaimMaxBytes   = len(windowsV3DirectoryClaimTag) + 4 +
		windowsV3VolumeGUIDClaimMaxBytes + 8 + len(windowsV3PersistentObjectID{})
	windowsV3FileBothDirectoryInfoClass        = 3
	windowsV3FileNamesInformation              = 12
	windowsV3FileNamesInformationHeader        = 12
	windowsV3FiletimeTicksPerSecond            = uint64(10_000_000)
	windowsV3FiletimeNanosecondsPerTick        = uint32(100)
	windowsV3UnixEpochFiletimeSeconds          = int64(11_644_473_600)
	windowsV3FinalPathNameOpened        uint32 = 0x8
)

// windowsV3FileBothDirectoryInfo mirrors FILE_BOTH_DIR_INFORMATION. Keeping the
// layout local lets admission inspect both names from the already-pinned parent;
// opening a child would incorrectly require access to a collision we will never
// overwrite and NtCreateFile does not permit a zero desired-access mask.
type windowsV3FileBothDirectoryInfo struct {
	nextEntryOffset uint32
	fileIndex       uint32
	creationTime    windows.Filetime
	lastAccessTime  windows.Filetime
	lastWriteTime   windows.Filetime
	changeTime      windows.Filetime
	endOfFile       uint64
	allocationSize  uint64
	fileAttributes  uint32
	fileNameLength  uint32
	eaSize          uint32
	shortNameLength uint8
	_               uint8
	shortName       [12]uint16
	fileName        [1]uint16
}

const (
	windowsV3FileBothDirectoryInfoHeader = int(unsafe.Offsetof(windowsV3FileBothDirectoryInfo{}.fileName))
	windowsV3FileBothDirectoryInfoAlign  = int(unsafe.Alignof(windowsV3FileBothDirectoryInfo{}))
)

type windowsV3LongAndShortName struct {
	long  string
	short string
}

var (
	windowsV3NativeDLL            = windows.NewLazySystemDLL("ntdll.dll")
	windowsV3NtQueryDirectoryFile = windowsV3NativeDLL.NewProc("NtQueryDirectoryFile")
	windowsV3RtlUpcaseUnicodeChar = windowsV3NativeDLL.NewProc("RtlUpcaseUnicodeChar")
)

func (directory *windowsV3Directory) names(limit int) ([]string, error) {
	return directory.namesMatching(limit, func(string) bool { return true })
}

func (directory *windowsV3Directory) namesWithPrefix(prefix string, limit int) ([]string, error) {
	foldedPrefix, err := windowsV3NTFSCaseKey(prefix)
	if err != nil {
		return nil, windowsV3Failure("canonicalize directory prefix", prefix, errWindowsV3OutputUnsupported, err)
	}
	var foldErr error
	names, scanErr := directory.namesMatching(limit, func(name string) bool {
		foldedName, err := windowsV3NTFSCaseKey(name)
		if err != nil {
			foldErr = err
			return false
		}
		return strings.HasPrefix(foldedName, foldedPrefix)
	})
	if foldErr != nil {
		foldErr = windowsV3Failure("canonicalize directory entry", prefix, errWindowsV3OutputUnsupported, foldErr)
	}
	return names, errors.Join(scanErr, foldErr)
}

func (directory *windowsV3Directory) namesMatching(
	limit int,
	include func(string) bool,
) ([]string, error) {
	const operation = "enumerate output directory"
	if limit < 0 || include == nil {
		return nil, windowsV3Failure(operation, "", errWindowsV3OutputUnsafe,
			errors.New("enumeration bound or filter is invalid"))
	}
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if err := directory.verify(false); err != nil {
		return nil, err
	}

	if directory.enumerate == nil {
		return nil, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe,
			errors.New("directory enumeration authority is absent"))
	}
	directory.enumerate.Lock()
	defer directory.enumerate.Unlock()
	names := make([]string, 0, min(limit, 16))
	restart := uintptr(1)
	buffer := make([]byte, windowsV3DirectoryReadBufferBytes)
	for {
		var status windows.IO_STATUS_BLOCK
		rawStatus, _, _ := windowsV3NtQueryDirectoryFile.Call(
			uintptr(directory.handle()),
			0,
			0,
			0,
			uintptr(unsafe.Pointer(&status)),
			uintptr(unsafe.Pointer(&buffer[0])),
			uintptr(len(buffer)),
			windowsV3FileNamesInformation,
			0,
			0,
			restart,
		)
		runtime.KeepAlive(directory)
		nativeStatus := windows.NTStatus(uint32(rawStatus))
		if nativeStatus == windows.STATUS_NO_MORE_FILES {
			break
		}
		if nativeStatus != 0 {
			return nil, windowsV3NativeOperationFailure(operation, directory.path, nativeStatus.Errno())
		}
		used := int(status.Information)
		if used <= 0 || used > len(buffer) {
			return nil, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe,
				errors.New("directory query returned an invalid byte count"))
		}
		batch, err := windowsV3ParseDirectoryNames(buffer[:used])
		if err != nil {
			return nil, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe, err)
		}
		for _, name := range batch {
			if !include(name) {
				continue
			}
			if len(names) == limit {
				return nil, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe,
					errors.New("directory exceeds its declared entry bound"))
			}
			names = append(names, name)
		}
		restart = 0
	}
	if err := directory.verify(false); err != nil {
		return nil, err
	}
	sort.Strings(names)
	for index := 1; index < len(names); index++ {
		if stringsEqualFoldExact(names[index-1], names[index]) {
			return nil, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe,
				errors.New("directory enumeration returned platform-equivalent duplicate names"))
		}
	}
	return names, nil
}

func windowsV3ParseDirectoryNames(buffer []byte) ([]string, error) {
	names := make([]string, 0, 16)
	for offset := 0; ; {
		if len(buffer)-offset < windowsV3FileNamesInformationHeader {
			return nil, errors.New("directory query returned a truncated entry header")
		}
		entry := buffer[offset:]
		next := int(binary.LittleEndian.Uint32(entry[0:4]))
		nameBytes := int(binary.LittleEndian.Uint32(entry[8:12]))
		if nameBytes == 0 || nameBytes%2 != 0 || nameBytes > len(entry)-windowsV3FileNamesInformationHeader {
			return nil, errors.New("directory query returned an invalid UTF-16 name length")
		}
		minimumEntryBytes := windowsV3FileNamesInformationHeader + nameBytes
		if next != 0 && (next%4 != 0 || next < minimumEntryBytes || next >= len(entry)) {
			return nil, errors.New("directory query returned an invalid next-entry offset")
		}
		units := make([]uint16, nameBytes/2)
		for index := range units {
			units[index] = binary.LittleEndian.Uint16(
				entry[windowsV3FileNamesInformationHeader+index*2:],
			)
		}
		if !windowsV3ValidUTF16Name(units) {
			return nil, errors.New("directory query returned a malformed UTF-16 name")
		}
		name := string(utf16.Decode(units))
		if name != "." && name != ".." {
			names = append(names, name)
		}
		if next == 0 {
			return names, nil
		}
		offset += next
	}
}

func windowsV3ValidUTF16Name(units []uint16) bool {
	for index := 0; index < len(units); index++ {
		switch {
		case units[index] == 0:
			return false
		case 0xd800 <= units[index] && units[index] <= 0xdbff:
			index++
			if index == len(units) || units[index] < 0xdc00 || units[index] > 0xdfff {
				return false
			}
		case 0xdc00 <= units[index] && units[index] <= 0xdfff:
			return false
		}
	}
	return true
}

func (directory *windowsV3Directory) observeEntry(relative string) (outputV3EntryKind, error) {
	kind, _, err := directory.classifyExactEntry(relative)
	return kind, err
}

func (directory *windowsV3Directory) classifyExactEntry(
	relative string,
) (_ outputV3EntryKind, exact bool, resultErr error) {
	observation, err := directory.inspectEntryName(relative)
	if err != nil {
		return outputV3EntryAbsent, false, err
	}
	return observation.kind,
		observation.kind == outputV3EntryAbsent || observation.actual == observation.requested, nil
}

func (directory *windowsV3Directory) validatePublicEntryName(relative string) error {
	return directory.validatePublicEntryNames([]string{relative})
}

type windowsV3PublicEntryAuthority struct {
	requested   string
	longCount   int
	aliasCount  int
	aliasActual string
}

func (directory *windowsV3Directory) validatePublicEntryNames(relatives []string) error {
	const operation = "validate public output entry name"
	if err := directory.usable(); err != nil {
		return err
	}
	authorities := make(map[string]*windowsV3PublicEntryAuthority, len(relatives))
	authorityOrder := make([]*windowsV3PublicEntryAuthority, 0, len(relatives))
	for _, relative := range relatives {
		requested, err := windowsV3RelativePath(relative, true)
		if err != nil {
			return windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe, err)
		}
		requestedKey, err := windowsV3NTFSCaseKey(requested)
		if err != nil {
			return windowsV3Failure(operation, relative, errWindowsV3OutputUnsupported, err)
		}
		if _, exists := authorities[requestedKey]; !exists {
			authority := &windowsV3PublicEntryAuthority{requested: requested}
			authorities[requestedKey] = authority
			authorityOrder = append(authorityOrder, authority)
		}
	}
	if len(authorities) == 0 {
		return nil
	}
	if err := directory.scanPublicEntryAuthorities(authorities); err != nil {
		return err
	}
	for _, authority := range authorityOrder {
		if authority.longCount > 1 || authority.aliasCount > 1 ||
			(authority.longCount != 0 && authority.aliasCount != 0) {
			return windowsV3Failure(operation, authority.requested, errWindowsV3OutputUnsafe,
				errors.New("directory contains ambiguous long and short name authority"))
		}
		if authority.aliasCount == 1 {
			return windowsV3Failure(operation, authority.requested, errWindowsV3OutputUnsafe,
				fmt.Errorf("requested entry resolves through a DOS alias to long leaf %q", authority.aliasActual))
		}
	}
	// A long-name match is safe regardless of the child's ACL or kind. No match is
	// equally safe: later fixed-parent opens still settle a racing entry as a
	// collision, while this preflight has not acquired access to file content.
	return nil
}

func (directory *windowsV3Directory) scanPublicEntryAuthorities(
	authorities map[string]*windowsV3PublicEntryAuthority,
) error {
	const operation = "enumerate output long and short names"
	if err := directory.verify(false); err != nil {
		return err
	}
	if directory.enumerate == nil {
		return windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe,
			errors.New("directory enumeration authority is absent"))
	}
	directory.enumerate.Lock()
	defer directory.enumerate.Unlock()

	restart := uintptr(1)
	// NtQueryDirectoryFile requires naturally aligned storage. A uint64 backing
	// allocation makes that authority explicit instead of relying on []byte heap
	// alignment as an implementation detail.
	aligned := make([]uint64, windowsV3DirectoryReadBufferBytes/8)
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&aligned[0])), windowsV3DirectoryReadBufferBytes)
	for {
		var status windows.IO_STATUS_BLOCK
		rawStatus, _, _ := windowsV3NtQueryDirectoryFile.Call(
			uintptr(directory.handle()),
			0,
			0,
			0,
			uintptr(unsafe.Pointer(&status)),
			uintptr(unsafe.Pointer(&buffer[0])),
			uintptr(len(buffer)),
			windowsV3FileBothDirectoryInfoClass,
			0,
			0,
			restart,
		)
		runtime.KeepAlive(directory)
		runtime.KeepAlive(aligned)
		nativeStatus := windows.NTStatus(uint32(rawStatus))
		if nativeStatus == windows.STATUS_NO_MORE_FILES || nativeStatus == windows.STATUS_NO_SUCH_FILE {
			break
		}
		if nativeStatus != 0 {
			return windowsV3NativeOperationFailure(operation, directory.path, nativeStatus.Errno())
		}
		used := int(status.Information)
		if used <= 0 || used > len(buffer) {
			return windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe,
				errors.New("directory query returned an invalid long/short-name byte count"))
		}
		entries, err := windowsV3ParseLongAndShortNames(buffer[:used])
		if err != nil {
			return windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe, err)
		}
		for _, entry := range entries {
			longKey, err := windowsV3NTFSCaseKey(entry.long)
			if err != nil {
				return err
			}
			if authority := authorities[longKey]; authority != nil {
				authority.longCount++
			}
			if entry.short == "" {
				continue
			}
			shortKey, err := windowsV3NTFSCaseKey(entry.short)
			if err != nil {
				return err
			}
			if authority := authorities[shortKey]; authority != nil && longKey != shortKey {
				authority.aliasCount++
				authority.aliasActual = entry.long
			}
		}
		restart = 0
	}
	if err := directory.verify(false); err != nil {
		return err
	}
	return nil
}

func windowsV3ParseLongAndShortNames(buffer []byte) ([]windowsV3LongAndShortName, error) {
	entries := make([]windowsV3LongAndShortName, 0, 16)
	for offset := 0; ; {
		if len(buffer)-offset < windowsV3FileBothDirectoryInfoHeader {
			return nil, errors.New("directory query returned a truncated long/short-name header")
		}
		info := (*windowsV3FileBothDirectoryInfo)(unsafe.Pointer(&buffer[offset]))
		nameBytes := int(info.fileNameLength)
		shortBytes := int(info.shortNameLength)
		if nameBytes <= 0 || nameBytes%2 != 0 ||
			nameBytes > len(buffer)-offset-windowsV3FileBothDirectoryInfoHeader {
			return nil, errors.New("directory query returned an invalid long-name length")
		}
		if shortBytes%2 != 0 || shortBytes > len(info.shortName)*2 {
			return nil, errors.New("directory query returned an invalid short-name length")
		}
		minimumEntryBytes := windowsV3FileBothDirectoryInfoHeader + nameBytes
		next := int(info.nextEntryOffset)
		if next != 0 && (next%windowsV3FileBothDirectoryInfoAlign != 0 ||
			next < minimumEntryBytes || next >= len(buffer)-offset) {
			return nil, errors.New("directory query returned an invalid long/short-name next-entry offset")
		}
		longUnits := unsafe.Slice(&info.fileName[0], nameBytes/2)
		if !windowsV3ValidUTF16Name(longUnits) {
			return nil, errors.New("directory query returned a malformed UTF-16 long name")
		}
		longName := string(utf16.Decode(longUnits))
		shortName := ""
		if shortBytes != 0 {
			shortUnits := info.shortName[:shortBytes/2]
			if !windowsV3ValidUTF16Name(shortUnits) {
				return nil, errors.New("directory query returned a malformed UTF-16 short name")
			}
			shortName = string(utf16.Decode(shortUnits))
		}
		if longName != "." && longName != ".." {
			entries = append(entries, windowsV3LongAndShortName{long: longName, short: shortName})
		}
		if next == 0 {
			return entries, nil
		}
		offset += next
	}
}

type windowsV3EntryNameObservation struct {
	kind      outputV3EntryKind
	requested string
	actual    string
}

func (directory *windowsV3Directory) inspectEntryName(
	relative string,
) (_ windowsV3EntryNameObservation, resultErr error) {
	const operation = "observe output entry"
	if err := directory.usable(); err != nil {
		return windowsV3EntryNameObservation{}, err
	}
	name, err := windowsV3RelativePath(relative, true)
	if err != nil {
		return windowsV3EntryNameObservation{}, windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe, err)
	}
	// The parent is already fixed and the name is exactly one component. Omitting
	// OBJ_DONT_REPARSE only for this read-only leaf observation lets a reparse
	// point be classified as a collision without ever following it.
	handle, status, err := windowsV3OpenNativeWithOptions(
		directory.handle(), name, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_OPEN, 0, 0, nil,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.OBJ_CASE_INSENSITIVE,
	)
	if errors.Is(err, fs.ErrNotExist) {
		return windowsV3EntryNameObservation{
			kind: outputV3EntryAbsent, requested: name, actual: name,
		}, nil
	}
	if err != nil {
		return windowsV3EntryNameObservation{}, windowsV3NativeOperationFailure(operation, relative, err)
	}
	if _, err := windowsV3CreationStatus(windows.FILE_OPEN, status); err != nil {
		_ = windows.CloseHandle(handle)
		return windowsV3EntryNameObservation{}, windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe, err)
	}
	opened := os.NewFile(uintptr(handle), relative)
	if opened == nil {
		_ = windows.CloseHandle(handle)
		return windowsV3EntryNameObservation{}, windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe,
			errors.New("wrap observed entry handle"))
	}
	defer func() { resultErr = errors.Join(resultErr, opened.Close()) }()
	actualName, err := windowsV3OpenedLeafName(handle)
	if err != nil {
		return windowsV3EntryNameObservation{}, windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe, err)
	}
	facts, err := directory.inspector.Inspect(handle)
	if err != nil || !strings.EqualFold(facts.filesystem, windowsV3OutputFilesystem) ||
		facts.object.volume != directory.volume || !facts.object.valid() {
		return windowsV3EntryNameObservation{}, windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe,
			errors.Join(errors.New("observed entry is outside the fixed NTFS authority"), err))
	}
	kind := outputV3EntryRegularFile
	if facts.attributes&windowsV3CloudAttributeMask != 0 {
		kind = outputV3EntryOther
	} else if facts.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		kind = outputV3EntryDirectory
	}
	return windowsV3EntryNameObservation{kind: kind, requested: name, actual: actualName}, nil
}

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
	const operation = "claim output directory identity"
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if err := directory.verifyPublicIdentityAuthority(); err != nil {
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
	kind       outputV3EntryKind
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
) (windowsV3ObjectIdentity, uint32, outputV3EntryKind, error) {
	var fileID windowsV3FileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&fileID)), uint32(unsafe.Sizeof(fileID)),
	); err != nil {
		return windowsV3ObjectIdentity{}, 0, outputV3EntryAbsent, err
	}
	identity := windowsV3ObjectIdentity{volume: expectedVolume, fileID: fileID.FileID}
	if fileID.VolumeSerialNumber != expectedVolume.serial || !identity.valid() {
		return windowsV3ObjectIdentity{}, 0, outputV3EntryAbsent,
			errors.New("entry is outside the fixed NTFS volume identity")
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsV3ObjectIdentity{}, 0, outputV3EntryAbsent, err
	}
	kind := outputV3EntryRegularFile
	if information.FileAttributes&windowsV3CloudAttributeMask != 0 {
		kind = outputV3EntryOther
	} else if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		kind = outputV3EntryDirectory
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
	if expected == nil || expected.kind != outputV3EntryDirectory ||
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

func windowsV3OutputLocatorKey(path string) (string, error) {
	canonical, err := catalog.CanonicalPath(path)
	if err != nil || canonical != path {
		return "", errors.Join(windowsV3Failure("validate output locator", path, errWindowsV3OutputUnsafe,
			errors.New("locator is not a canonical catalog path")), err)
	}
	native, err := windowsV3RelativePath(path, false)
	if err != nil {
		return "", windowsV3Failure("validate output locator", path, errWindowsV3OutputUnsafe, err)
	}
	key, err := windowsV3NTFSCaseKey(native)
	if err != nil {
		return "", windowsV3Failure("canonicalize output locator", path, errWindowsV3OutputUnsupported, err)
	}
	return key, nil
}

func windowsV3NTFSCaseKey(value string) (string, error) {
	if err := windowsV3RtlUpcaseUnicodeChar.Find(); err != nil {
		return "", fmt.Errorf("load Windows ordinal upcase table: %w", err)
	}
	units := utf16.Encode([]rune(value))
	for index, unit := range units {
		upper, _, _ := windowsV3RtlUpcaseUnicodeChar.Call(uintptr(unit))
		units[index] = uint16(upper)
	}
	return string(utf16.Decode(units)), nil
}

func windowsV3VerifyOpenedExactName(handle windows.Handle, expected string) error {
	return windowsV3VerifyOpenedLeafAuthority(handle, expected, true)
}

func windowsV3VerifyOpenedPlacementLeafAuthority(
	handle windows.Handle,
	requested string,
	parentCaseSensitive bool,
) error {
	const operation = "verify opened output placement name"
	native, err := windowsV3RelativePath(requested, true)
	if err != nil {
		return windowsV3Failure(operation, requested, errWindowsV3OutputUnsafe, err)
	}
	normalized, err := windowsV3OpenedLeafNameWithFlags(handle, 0)
	if err != nil {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsafe, err)
	}
	opened, err := windowsV3OpenedLeafNameWithFlags(handle, windowsV3FinalPathNameOpened)
	if err != nil {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsafe, err)
	}
	match, compareErr := windowsV3PlacementLeafNamesMatch(
		native, normalized, opened, parentCaseSensitive,
	)
	if compareErr != nil {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsupported, compareErr)
	}
	if !match {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsafe,
			fmt.Errorf("requested placement resolves to normalized leaf %q through opened leaf %q", normalized, opened))
	}
	return nil
}

func windowsV3PlacementLeafNamesMatch(requested, normalized, opened string, caseSensitive bool) (bool, error) {
	if caseSensitive {
		return requested == normalized || requested == opened, nil
	}
	requestedKey, requestedErr := windowsV3NTFSCaseKey(requested)
	normalizedKey, normalizedErr := windowsV3NTFSCaseKey(normalized)
	openedKey, openedErr := windowsV3NTFSCaseKey(opened)
	if requestedErr != nil || normalizedErr != nil || openedErr != nil {
		return false, errors.Join(requestedErr, normalizedErr, openedErr)
	}
	return requestedKey == normalizedKey || requestedKey == openedKey, nil
}

func windowsV3VerifyOpenedLeafAuthority(handle windows.Handle, expected string, exact bool) error {
	const operation = "verify opened output entry name"
	native, err := windowsV3RelativePath(expected, true)
	if err != nil {
		return windowsV3Failure(operation, expected, errWindowsV3OutputUnsafe, err)
	}
	actual, err := windowsV3OpenedLeafName(handle)
	if err != nil {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsafe, err)
	}
	if exact && actual != native {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsafe,
			fmt.Errorf("actual private entry spelling is %q", actual))
	}
	if exact {
		return nil
	}
	// NTFS can resolve a DOS 8.3 alias that is unrelated to the requested
	// catalog component. Comparing the handle-derived long leaf through the
	// volume's ordinal upcase table permits case-only reuse while refusing a
	// different namespace spelling before descent or mutation.
	expectedKey, expectedErr := windowsV3NTFSCaseKey(native)
	actualKey, actualErr := windowsV3NTFSCaseKey(actual)
	if expectedErr != nil || actualErr != nil {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsupported,
			errors.Join(expectedErr, actualErr))
	}
	if actualKey != expectedKey {
		return windowsV3Failure(operation, native, errWindowsV3OutputUnsafe,
			fmt.Errorf("requested entry resolves to different long leaf %q", actual))
	}
	return nil
}

func windowsV3OpenedLeafName(handle windows.Handle) (string, error) {
	return windowsV3OpenedLeafNameWithFlags(handle, 0)
}

func windowsV3OpenedLeafNameWithFlags(handle windows.Handle, flags uint32) (string, error) {
	// Both normalized and opened spellings come from the same handle. Callers can
	// therefore distinguish canonical output lookup from placement-only alias
	// binding without re-resolving an attacker-controlled path.
	normalized, err := windowsV3FinalPath(handle, flags)
	if err != nil {
		return "", fmt.Errorf("query opened file name: %w", err)
	}
	leaf := filepath.Base(normalized)
	if leaf == "." || leaf == string(filepath.Separator) {
		return "", errors.New("opened file name does not contain a leaf component")
	}
	validated, err := windowsV3RelativePath(leaf, true)
	if err != nil {
		return "", fmt.Errorf("validate opened file name: %w", err)
	}
	return validated, nil
}

func windowsV3ValidateModifiedTime(modified catalog.ModifiedTime) error {
	_, _, err := windowsV3ModifiedTimeTicks(modified)
	return err
}

func windowsV3ModifiedTimeTicks(modified catalog.ModifiedTime) (uint64, bool, error) {
	const operation = "validate output modified time"
	if !modified.Present() {
		return 0, false, nil
	}
	if modified.Precision() < catalog.TimePrecisionSeconds || modified.Precision() > catalog.TimePrecisionNanoseconds ||
		modified.Nanoseconds() >= 1_000_000_000 {
		return 0, false, windowsV3Failure(operation, "", errWindowsV3OutputUnsupported,
			errors.New("catalog modified time is invalid"))
	}
	if modified.Precision() == catalog.TimePrecisionNanoseconds &&
		modified.Nanoseconds()%windowsV3FiletimeNanosecondsPerTick != 0 {
		return 0, false, windowsV3Failure(operation, "", errWindowsV3OutputUnsupported,
			errors.New("nanosecond timestamp is not exactly representable by NTFS FILETIME"))
	}
	shiftedSeconds := modified.Seconds() + windowsV3UnixEpochFiletimeSeconds
	if shiftedSeconds < 0 {
		return 0, false, windowsV3Failure(operation, "", errWindowsV3OutputUnsupported,
			errors.New("timestamp predates the NTFS FILETIME epoch"))
	}
	seconds := uint64(shiftedSeconds)
	fraction := uint64(modified.Nanoseconds() / windowsV3FiletimeNanosecondsPerTick)
	if seconds > math.MaxUint64/windowsV3FiletimeTicksPerSecond ||
		seconds*windowsV3FiletimeTicksPerSecond > math.MaxUint64-fraction {
		return 0, false, windowsV3Failure(operation, "", errWindowsV3OutputUnsupported,
			errors.New("timestamp exceeds the NTFS FILETIME range"))
	}
	return seconds*windowsV3FiletimeTicksPerSecond + fraction, true, nil
}

func (file *windowsV3File) setModifiedTime(modified catalog.ModifiedTime) error {
	if err := file.verify(false); err != nil {
		return err
	}
	return windowsV3SetHandleModifiedTime(file.handle(), file.path, modified)
}

func (directory *windowsV3Directory) setModifiedTime(modified catalog.ModifiedTime) error {
	if err := directory.verify(false); err != nil {
		return err
	}
	return windowsV3SetHandleModifiedTime(directory.handle(), directory.path, modified)
}

func windowsV3SetHandleModifiedTime(handle windows.Handle, path string, modified catalog.ModifiedTime) error {
	ticks, present, err := windowsV3ModifiedTimeTicks(modified)
	if err != nil || !present {
		return err
	}
	filetime := windows.Filetime{LowDateTime: uint32(ticks), HighDateTime: uint32(ticks >> 32)}
	if err := windows.SetFileTime(handle, nil, nil, &filetime); err != nil {
		return windowsV3NativeOperationFailure("set output modified time", path, err)
	}
	return nil
}

func (file *windowsV3File) metadataMatches(
	exactSize uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	if err := file.verify(false); err != nil {
		return false, err
	}
	metadata, err := windowsV3ReadHandleMetadata(file.handle())
	if err != nil {
		return false, windowsV3Failure("inspect output file metadata", file.path, errWindowsV3OutputUnsafe, err)
	}
	return metadata.size == exactSize && windowsV3ModifiedTimeMatches(metadata.modifiedTicks, modified), nil
}

func windowsV3ReadHandleMetadata(handle windows.Handle) (windowsV3OutputMetadata, error) {
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsV3OutputMetadata{}, err
	}
	return windowsV3OutputMetadata{
		size: uint64(information.FileSizeHigh)<<32 | uint64(information.FileSizeLow),
		modifiedTicks: uint64(information.LastWriteTime.HighDateTime)<<32 |
			uint64(information.LastWriteTime.LowDateTime),
	}, nil
}

func windowsV3ModifiedTimeMatches(actual uint64, expected catalog.ModifiedTime) bool {
	ticks, present, err := windowsV3ModifiedTimeTicks(expected)
	if err != nil {
		return false
	}
	if !present {
		return true
	}
	switch expected.Precision() {
	case catalog.TimePrecisionSeconds:
		return actual/windowsV3FiletimeTicksPerSecond == ticks/windowsV3FiletimeTicksPerSecond
	case catalog.TimePrecisionMilliseconds:
		const ticksPerMillisecond = uint64(10_000)
		return actual/ticksPerMillisecond == ticks/ticksPerMillisecond
	case catalog.TimePrecisionNanoseconds:
		return actual == ticks
	default:
		return false
	}
}
