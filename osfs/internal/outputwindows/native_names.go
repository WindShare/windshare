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

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

const (
	windowsDirectoryReadBufferBytes = 32 << 10
	windowsDirectoryClaimTag        = "windows/ntfs/directory-object/v2"
	windowsVolumeGUIDClaimMaxBytes  = 128
	windowsDirectoryClaimMaxBytes   = len(windowsDirectoryClaimTag) + 4 +
		windowsVolumeGUIDClaimMaxBytes + 8 + len(windowsPersistentObjectID{})
	windowsFileBothDirectoryInfoClass        = 3
	windowsFileNamesInformation              = 12
	windowsFileNamesInformationHeader        = 12
	windowsFiletimeTicksPerSecond            = uint64(10_000_000)
	windowsFiletimeNanosecondsPerTick        = uint32(100)
	windowsUnixEpochFiletimeSeconds          = int64(11_644_473_600)
	windowsFinalPathNameOpened        uint32 = 0x8
)

// windowsFileBothDirectoryInfo mirrors FILE_BOTH_DIR_INFORMATION. Keeping the
// layout local lets admission inspect both names from the already-pinned parent;
// opening a child would incorrectly require access to a collision we will never
// overwrite and NtCreateFile does not permit a zero desired-access mask.
type windowsFileBothDirectoryInfo struct {
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
	windowsFileBothDirectoryInfoHeader = int(unsafe.Offsetof(windowsFileBothDirectoryInfo{}.fileName))
	windowsFileBothDirectoryInfoAlign  = int(unsafe.Alignof(windowsFileBothDirectoryInfo{}))
)

type windowsLongAndShortName struct {
	long  string
	short string
}

var (
	windowsNativeDLL            = windows.NewLazySystemDLL("ntdll.dll")
	windowsNtQueryDirectoryFile = windowsNativeDLL.NewProc("NtQueryDirectoryFile")
	windowsRtlUpcaseUnicodeChar = windowsNativeDLL.NewProc("RtlUpcaseUnicodeChar")
)

func (directory *windowsDirectory) names(limit int) ([]string, error) {
	return directory.namesMatching(limit, func(string) bool { return true })
}

func (directory *windowsDirectory) namesWithPrefix(prefix string, limit int) ([]string, error) {
	foldedPrefix, err := windowsNTFSCaseKey(prefix)
	if err != nil {
		return nil, windowsFailure("canonicalize directory prefix", prefix, errWindowsOutputUnsupported, err)
	}
	var foldErr error
	names, scanErr := directory.namesMatching(limit, func(name string) bool {
		foldedName, err := windowsNTFSCaseKey(name)
		if err != nil {
			foldErr = err
			return false
		}
		return strings.HasPrefix(foldedName, foldedPrefix)
	})
	if foldErr != nil {
		foldErr = windowsFailure("canonicalize directory entry", prefix, errWindowsOutputUnsupported, foldErr)
	}
	return names, errors.Join(scanErr, foldErr)
}

func (directory *windowsDirectory) namesMatching(
	limit int,
	include func(string) bool,
) ([]string, error) {
	batch, err := directory.readNameBatch(limit, include)
	if err == nil && batch.HasMore {
		err = windowsFailure("enumerate output directory", directory.path, errWindowsOutputUnsafe,
			errors.New("directory exceeds its declared entry bound"))
	}
	if err != nil {
		return nil, err
	}
	return batch.Names, nil
}

func (directory *windowsDirectory) readNameBatch(
	limit int,
	include func(string) bool,
) (outputcap.DirectoryNameBatch, error) {
	const operation = "enumerate output directory"
	if limit < 0 || include == nil {
		return outputcap.DirectoryNameBatch{}, windowsFailure(operation, "", errWindowsOutputUnsafe,
			errors.New("enumeration bound or filter is invalid"))
	}
	if err := directory.usable(); err != nil {
		return outputcap.DirectoryNameBatch{}, err
	}
	if err := directory.verify(false); err != nil {
		return outputcap.DirectoryNameBatch{}, err
	}

	if directory.enumerate == nil {
		return outputcap.DirectoryNameBatch{}, windowsFailure(operation, directory.path, errWindowsOutputUnsafe,
			errors.New("directory enumeration authority is absent"))
	}
	directory.enumerate.Lock()
	defer directory.enumerate.Unlock()
	names := make([]string, 0, min(limit, 16))
	restart := uintptr(1)
	buffer := make([]byte, windowsDirectoryReadBufferBytes)
	hasMore := false
scan:
	for {
		var status windows.IO_STATUS_BLOCK
		rawStatus, _, _ := windowsNtQueryDirectoryFile.Call(
			uintptr(directory.handle()),
			0,
			0,
			0,
			uintptr(unsafe.Pointer(&status)),
			uintptr(unsafe.Pointer(&buffer[0])),
			uintptr(len(buffer)),
			windowsFileNamesInformation,
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
			return outputcap.DirectoryNameBatch{}, windowsNativeOperationFailure(operation, directory.path, nativeStatus.Errno())
		}
		used := int(status.Information)
		if used <= 0 || used > len(buffer) {
			return outputcap.DirectoryNameBatch{}, windowsFailure(operation, directory.path, errWindowsOutputUnsafe,
				errors.New("directory query returned an invalid byte count"))
		}
		batch, err := windowsParseDirectoryNames(buffer[:used])
		if err != nil {
			return outputcap.DirectoryNameBatch{}, windowsFailure(operation, directory.path, errWindowsOutputUnsafe, err)
		}
		for _, name := range batch {
			if !include(name) {
				continue
			}
			if len(names) == limit {
				hasMore = true
				break scan
			}
			names = append(names, name)
		}
		restart = 0
	}
	if err := directory.verify(false); err != nil {
		return outputcap.DirectoryNameBatch{}, err
	}
	sort.Strings(names)
	for index := 1; index < len(names); index++ {
		if stringsEqualFoldExact(names[index-1], names[index]) {
			return outputcap.DirectoryNameBatch{}, windowsFailure(operation, directory.path, errWindowsOutputUnsafe,
				errors.New("directory enumeration returned platform-equivalent duplicate names"))
		}
	}
	return outputcap.DirectoryNameBatch{Names: names, HasMore: hasMore}, nil
}

func windowsParseDirectoryNames(buffer []byte) ([]string, error) {
	names := make([]string, 0, 16)
	for offset := 0; ; {
		if len(buffer)-offset < windowsFileNamesInformationHeader {
			return nil, errors.New("directory query returned a truncated entry header")
		}
		entry := buffer[offset:]
		next := int(binary.LittleEndian.Uint32(entry[0:4]))
		nameBytes := int(binary.LittleEndian.Uint32(entry[8:12]))
		if nameBytes == 0 || nameBytes%2 != 0 || nameBytes > len(entry)-windowsFileNamesInformationHeader {
			return nil, errors.New("directory query returned an invalid UTF-16 name length")
		}
		minimumEntryBytes := windowsFileNamesInformationHeader + nameBytes
		if next != 0 && (next%4 != 0 || next < minimumEntryBytes || next >= len(entry)) {
			return nil, errors.New("directory query returned an invalid next-entry offset")
		}
		units := make([]uint16, nameBytes/2)
		for index := range units {
			units[index] = binary.LittleEndian.Uint16(
				entry[windowsFileNamesInformationHeader+index*2:],
			)
		}
		if !windowsValidUTF16Name(units) {
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

func windowsValidUTF16Name(units []uint16) bool {
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

func (directory *windowsDirectory) observeEntry(relative string) (outputcap.EntryKind, error) {
	kind, _, err := directory.classifyExactEntry(relative)
	return kind, err
}

func (directory *windowsDirectory) classifyExactEntry(
	relative string,
) (_ outputcap.EntryKind, exact bool, resultErr error) {
	observation, err := directory.inspectEntryName(relative)
	if err != nil {
		return outputcap.EntryAbsent, false, err
	}
	return observation.kind,
		observation.kind == outputcap.EntryAbsent || observation.actual == observation.requested, nil
}

func (directory *windowsDirectory) validatePublicEntryName(relative string) error {
	return directory.validatePublicEntryNames([]string{relative})
}

type windowsPublicEntryAuthority struct {
	requested   string
	longCount   int
	aliasCount  int
	aliasActual string
}

func (directory *windowsDirectory) validatePublicEntryNames(relatives []string) error {
	const operation = "validate public output entry name"
	if err := directory.usable(); err != nil {
		return err
	}
	authorities := make(map[string]*windowsPublicEntryAuthority, len(relatives))
	authorityOrder := make([]*windowsPublicEntryAuthority, 0, len(relatives))
	for _, relative := range relatives {
		requested, err := windowsRelativePath(relative, true)
		if err != nil {
			return windowsFailure(operation, relative, errWindowsOutputUnsafe, err)
		}
		requestedKey, err := windowsNTFSCaseKey(requested)
		if err != nil {
			return windowsFailure(operation, relative, errWindowsOutputUnsupported, err)
		}
		if _, exists := authorities[requestedKey]; !exists {
			authority := &windowsPublicEntryAuthority{requested: requested}
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
			return windowsFailure(operation, authority.requested, errWindowsOutputUnsafe,
				errors.New("directory contains ambiguous long and short name authority"))
		}
		if authority.aliasCount == 1 {
			return windowsFailure(operation, authority.requested, errWindowsOutputUnsafe,
				fmt.Errorf("requested entry resolves through a DOS alias to long leaf %q", authority.aliasActual))
		}
	}
	// A long-name match is safe regardless of the child's ACL or kind. No match is
	// equally safe: later fixed-parent opens still settle a racing entry as a
	// collision, while this preflight has not acquired access to file content.
	return nil
}

func (directory *windowsDirectory) scanPublicEntryAuthorities(
	authorities map[string]*windowsPublicEntryAuthority,
) error {
	const operation = "enumerate output long and short names"
	if err := directory.verify(false); err != nil {
		return err
	}
	if directory.enumerate == nil {
		return windowsFailure(operation, directory.path, errWindowsOutputUnsafe,
			errors.New("directory enumeration authority is absent"))
	}
	directory.enumerate.Lock()
	defer directory.enumerate.Unlock()

	restart := uintptr(1)
	// NtQueryDirectoryFile requires naturally aligned storage. A uint64 backing
	// allocation makes that authority explicit instead of relying on []byte heap
	// alignment as an implementation detail.
	aligned := make([]uint64, windowsDirectoryReadBufferBytes/8)
	for {
		entries, done, err := directory.queryLongAndShortNames(aligned, restart)
		if err != nil {
			return err
		}
		if done {
			break
		}
		if err := windowsRecordPublicEntryAuthorities(authorities, entries); err != nil {
			return err
		}
		restart = 0
	}
	if err := directory.verify(false); err != nil {
		return err
	}
	return nil
}

func (directory *windowsDirectory) queryLongAndShortNames(
	aligned []uint64,
	restart uintptr,
) ([]windowsLongAndShortName, bool, error) {
	const operation = "enumerate output long and short names"
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&aligned[0])), windowsDirectoryReadBufferBytes)
	var status windows.IO_STATUS_BLOCK
	rawStatus, _, _ := windowsNtQueryDirectoryFile.Call(
		uintptr(directory.handle()),
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&status)),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
		windowsFileBothDirectoryInfoClass,
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
		return nil, false, windowsNativeOperationFailure(operation, directory.path, nativeStatus.Errno())
	}
	used := int(status.Information)
	if used <= 0 || used > len(buffer) {
		return nil, false, windowsFailure(operation, directory.path, errWindowsOutputUnsafe,
			errors.New("directory query returned an invalid long/short-name byte count"))
	}
	entries, err := windowsParseLongAndShortNames(buffer[:used])
	if err != nil {
		return nil, false, windowsFailure(operation, directory.path, errWindowsOutputUnsafe, err)
	}
	return entries, false, nil
}

func windowsRecordPublicEntryAuthorities(
	authorities map[string]*windowsPublicEntryAuthority,
	entries []windowsLongAndShortName,
) error {
	for _, entry := range entries {
		longKey, err := windowsNTFSCaseKey(entry.long)
		if err != nil {
			return err
		}
		if authority := authorities[longKey]; authority != nil {
			authority.longCount++
		}
		if entry.short == "" {
			continue
		}
		shortKey, err := windowsNTFSCaseKey(entry.short)
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

func windowsParseLongAndShortNames(buffer []byte) ([]windowsLongAndShortName, error) {
	entries := make([]windowsLongAndShortName, 0, 16)
	for offset := 0; ; {
		if len(buffer)-offset < windowsFileBothDirectoryInfoHeader {
			return nil, errors.New("directory query returned a truncated long/short-name header")
		}
		info := (*windowsFileBothDirectoryInfo)(unsafe.Pointer(&buffer[offset]))
		nameBytes := int(info.fileNameLength)
		shortBytes := int(info.shortNameLength)
		if nameBytes <= 0 || nameBytes%2 != 0 ||
			nameBytes > len(buffer)-offset-windowsFileBothDirectoryInfoHeader {
			return nil, errors.New("directory query returned an invalid long-name length")
		}
		if shortBytes%2 != 0 || shortBytes > len(info.shortName)*2 {
			return nil, errors.New("directory query returned an invalid short-name length")
		}
		minimumEntryBytes := windowsFileBothDirectoryInfoHeader + nameBytes
		next := int(info.nextEntryOffset)
		if next != 0 && (next%windowsFileBothDirectoryInfoAlign != 0 ||
			next < minimumEntryBytes || next >= len(buffer)-offset) {
			return nil, errors.New("directory query returned an invalid long/short-name next-entry offset")
		}
		longUnits := unsafe.Slice(&info.fileName[0], nameBytes/2)
		if !windowsValidUTF16Name(longUnits) {
			return nil, errors.New("directory query returned a malformed UTF-16 long name")
		}
		longName := string(utf16.Decode(longUnits))
		shortName := ""
		if shortBytes != 0 {
			shortUnits := info.shortName[:shortBytes/2]
			if !windowsValidUTF16Name(shortUnits) {
				return nil, errors.New("directory query returned a malformed UTF-16 short name")
			}
			shortName = string(utf16.Decode(shortUnits))
		}
		if longName != "." && longName != ".." {
			entries = append(entries, windowsLongAndShortName{long: longName, short: shortName})
		}
		if next == 0 {
			return entries, nil
		}
		offset += next
	}
}

type windowsEntryNameObservation struct {
	kind      outputcap.EntryKind
	requested string
	actual    string
}

func (directory *windowsDirectory) inspectEntryName(
	relative string,
) (_ windowsEntryNameObservation, resultErr error) {
	const operation = "observe output entry"
	if err := directory.usable(); err != nil {
		return windowsEntryNameObservation{}, err
	}
	name, err := windowsRelativePath(relative, true)
	if err != nil {
		return windowsEntryNameObservation{}, windowsFailure(operation, relative, errWindowsOutputUnsafe, err)
	}
	// The parent is already fixed and the name is exactly one component. Omitting
	// OBJ_DONT_REPARSE only for this read-only leaf observation lets a reparse
	// point be classified as a collision without ever following it.
	handle, status, err := windowsOpenNativeWithOptions(
		directory.handle(), name, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_OPEN, 0, 0, nil,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.OBJ_CASE_INSENSITIVE,
	)
	if errors.Is(err, fs.ErrNotExist) {
		return windowsEntryNameObservation{
			kind: outputcap.EntryAbsent, requested: name, actual: name,
		}, nil
	}
	if err != nil {
		return windowsEntryNameObservation{}, windowsNativeOperationFailure(operation, relative, err)
	}
	if _, err := windowsCreationStatus(windows.FILE_OPEN, status); err != nil {
		_ = windows.CloseHandle(handle)
		return windowsEntryNameObservation{}, windowsFailure(operation, relative, errWindowsOutputUnsafe, err)
	}
	opened := os.NewFile(uintptr(handle), relative)
	if opened == nil {
		_ = windows.CloseHandle(handle)
		return windowsEntryNameObservation{}, windowsFailure(operation, relative, errWindowsOutputUnsafe,
			errors.New("wrap observed entry handle"))
	}
	defer func() { resultErr = errors.Join(resultErr, opened.Close()) }()
	actualName, err := windowsOpenedLeafName(handle)
	if err != nil {
		return windowsEntryNameObservation{}, windowsFailure(operation, relative, errWindowsOutputUnsafe, err)
	}
	facts, err := directory.inspector.Inspect(handle)
	if err != nil || facts.object.volume != directory.volume || !facts.object.valid() {
		return windowsEntryNameObservation{}, windowsFailure(operation, relative, errWindowsOutputUnsafe,
			errors.Join(errors.New("observed entry is outside the fixed NTFS authority"), err))
	}
	kind := outputcap.EntryRegularFile
	if facts.attributes&windowsCloudAttributeMask != 0 {
		kind = outputcap.EntryOther
	} else if facts.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		kind = outputcap.EntryDirectory
	}
	return windowsEntryNameObservation{kind: kind, requested: name, actual: actualName}, nil
}
