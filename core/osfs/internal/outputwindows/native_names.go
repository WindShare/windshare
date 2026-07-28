//go:build windows

package outputwindows

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"sort"
	"strings"
	"unicode/utf16"
	"unsafe"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
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

func (directory *windowsV3Directory) observeEntry(relative string) (outputcap.EntryKind, error) {
	kind, _, err := directory.classifyExactEntry(relative)
	return kind, err
}

func (directory *windowsV3Directory) classifyExactEntry(
	relative string,
) (_ outputcap.EntryKind, exact bool, resultErr error) {
	observation, err := directory.inspectEntryName(relative)
	if err != nil {
		return outputcap.EntryAbsent, false, err
	}
	return observation.kind,
		observation.kind == outputcap.EntryAbsent || observation.actual == observation.requested, nil
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
	for {
		entries, done, err := directory.queryLongAndShortNames(aligned, restart)
		if err != nil {
			return err
		}
		if done {
			break
		}
		if err := windowsV3RecordPublicEntryAuthorities(authorities, entries); err != nil {
			return err
		}
		restart = 0
	}
	if err := directory.verify(false); err != nil {
		return err
	}
	return nil
}

func (directory *windowsV3Directory) queryLongAndShortNames(
	aligned []uint64,
	restart uintptr,
) ([]windowsV3LongAndShortName, bool, error) {
	const operation = "enumerate output long and short names"
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&aligned[0])), windowsV3DirectoryReadBufferBytes)
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
		return nil, true, nil
	}
	if nativeStatus != 0 {
		return nil, false, windowsV3NativeOperationFailure(operation, directory.path, nativeStatus.Errno())
	}
	used := int(status.Information)
	if used <= 0 || used > len(buffer) {
		return nil, false, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe,
			errors.New("directory query returned an invalid long/short-name byte count"))
	}
	entries, err := windowsV3ParseLongAndShortNames(buffer[:used])
	if err != nil {
		return nil, false, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe, err)
	}
	return entries, false, nil
}

func windowsV3RecordPublicEntryAuthorities(
	authorities map[string]*windowsV3PublicEntryAuthority,
	entries []windowsV3LongAndShortName,
) error {
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
	kind      outputcap.EntryKind
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
			kind: outputcap.EntryAbsent, requested: name, actual: name,
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
	kind := outputcap.EntryRegularFile
	if facts.attributes&windowsV3CloudAttributeMask != 0 {
		kind = outputcap.EntryOther
	} else if facts.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		kind = outputcap.EntryDirectory
	}
	return windowsV3EntryNameObservation{kind: kind, requested: name, actual: actualName}, nil
}
