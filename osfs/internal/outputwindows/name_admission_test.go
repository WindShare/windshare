//go:build windows

package outputwindows

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
	"unsafe"
)

func TestWindowsParseLongAndShortNamesUsesNativeFILEBOTHLayout(t *testing.T) {
	buffer := windowsTestFileBothDirectoryBuffer(t, ".windshare-output", "WINDSH~1", 0)
	entries, err := windowsParseLongAndShortNames(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].long != ".windshare-output" || entries[0].short != "WINDSH~1" {
		t.Fatalf("parsed long/short names = %+v", entries)
	}
}

func TestWindowsParseLongAndShortNamesRejectsMisalignedNextEntry(t *testing.T) {
	if windowsFileBothDirectoryInfoAlign <= 4 {
		t.Skip("native FILE_BOTH layout requires only four-byte alignment")
	}
	const fourButNotEightAlignedOffset = 100
	buffer := windowsTestFileBothDirectoryBuffer(t, "a", "", fourButNotEightAlignedOffset)
	if _, err := windowsParseLongAndShortNames(buffer); err == nil {
		t.Fatal("parser accepted a next-entry offset that misaligns the native structure")
	}
}

func TestWindowsParseDirectoryNamesRejectsMalformedNativeRecords(t *testing.T) {
	valid := windowsDirectoryNamesBuffer("entry")
	for _, test := range []struct {
		name  string
		build func() []byte
	}{
		{name: "truncated header", build: func() []byte { return make([]byte, windowsFileNamesInformationHeader-1) }},
		{name: "zero name length", build: func() []byte {
			buffer := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint32(buffer[8:12], 0)
			return buffer
		}},
		{name: "odd name length", build: func() []byte {
			buffer := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint32(buffer[8:12], 1)
			return buffer
		}},
		{name: "oversized name length", build: func() []byte {
			buffer := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint32(buffer[8:12], uint32(len(buffer)))
			return buffer
		}},
		{name: "misaligned next offset", build: func() []byte {
			buffer := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint32(buffer[0:4], 13)
			return buffer
		}},
		{name: "short next offset", build: func() []byte {
			buffer := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint32(buffer[0:4], 4)
			return buffer
		}},
		{name: "next offset beyond buffer", build: func() []byte {
			buffer := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint32(buffer[0:4], uint32(len(buffer)))
			return buffer
		}},
		{name: "embedded NUL", build: func() []byte {
			buffer := windowsDirectoryNamesBuffer("a")
			binary.LittleEndian.PutUint16(buffer[windowsFileNamesInformationHeader:], 0)
			return buffer
		}},
		{name: "lone high surrogate", build: func() []byte {
			buffer := windowsDirectoryNamesBuffer("a")
			binary.LittleEndian.PutUint16(buffer[windowsFileNamesInformationHeader:], 0xd800)
			return buffer
		}},
		{name: "lone low surrogate", build: func() []byte {
			buffer := windowsDirectoryNamesBuffer("a")
			binary.LittleEndian.PutUint16(buffer[windowsFileNamesInformationHeader:], 0xdc00)
			return buffer
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := windowsParseDirectoryNames(test.build()); err == nil {
				t.Fatal("malformed native directory record was accepted")
			}
		})
	}
}

func TestWindowsParseDirectoryNamesFiltersDotEntriesAndChainsRecords(t *testing.T) {
	buffer := windowsDirectoryNamesBuffer(".", "..", "visible")
	names, err := windowsParseDirectoryNames(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "visible" {
		t.Fatalf("directory names = %v, want [visible]", names)
	}
}

func TestWindowsParseLongAndShortNamesRejectsMalformedNativeRecords(t *testing.T) {
	for _, test := range []struct {
		name  string
		build func(t *testing.T) []byte
	}{
		{name: "truncated header", build: func(t *testing.T) []byte {
			return make([]byte, windowsFileBothDirectoryInfoHeader-1)
		}},
		{name: "zero long length", build: func(t *testing.T) []byte {
			buffer := windowsTestFileBothDirectoryBuffer(t, "a", "", 0)
			info := (*windowsFileBothDirectoryInfo)(unsafe.Pointer(&buffer[0]))
			info.fileNameLength = 0
			return buffer
		}},
		{name: "odd long length", build: func(t *testing.T) []byte {
			buffer := windowsTestFileBothDirectoryBuffer(t, "a", "", 0)
			info := (*windowsFileBothDirectoryInfo)(unsafe.Pointer(&buffer[0]))
			info.fileNameLength = 1
			return buffer
		}},
		{name: "oversized long length", build: func(t *testing.T) []byte {
			buffer := windowsTestFileBothDirectoryBuffer(t, "a", "", 0)
			info := (*windowsFileBothDirectoryInfo)(unsafe.Pointer(&buffer[0]))
			info.fileNameLength = uint32(len(buffer))
			return buffer
		}},
		{name: "odd short length", build: func(t *testing.T) []byte {
			buffer := windowsTestFileBothDirectoryBuffer(t, "a", "B", 0)
			info := (*windowsFileBothDirectoryInfo)(unsafe.Pointer(&buffer[0]))
			info.shortNameLength = 1
			return buffer
		}},
		{name: "oversized short length", build: func(t *testing.T) []byte {
			buffer := windowsTestFileBothDirectoryBuffer(t, "a", "B", 0)
			info := (*windowsFileBothDirectoryInfo)(unsafe.Pointer(&buffer[0]))
			info.shortNameLength = uint8(len(info.shortName)*2 + 2)
			return buffer
		}},
		{name: "malformed long UTF16", build: func(t *testing.T) []byte {
			buffer := windowsTestFileBothDirectoryBuffer(t, "a", "", 0)
			info := (*windowsFileBothDirectoryInfo)(unsafe.Pointer(&buffer[0]))
			info.fileName[0] = 0
			return buffer
		}},
		{name: "malformed short UTF16", build: func(t *testing.T) []byte {
			buffer := windowsTestFileBothDirectoryBuffer(t, "a", "B", 0)
			info := (*windowsFileBothDirectoryInfo)(unsafe.Pointer(&buffer[0]))
			info.shortName[0] = 0xdc00
			return buffer
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := windowsParseLongAndShortNames(test.build(t)); err == nil {
				t.Fatal("malformed native long/short record was accepted")
			}
		})
	}
}

func TestWindowsRecordPublicEntryAuthoritiesTracksLongAndAliasClaims(t *testing.T) {
	authorities := map[string]*windowsPublicEntryAuthority{
		"LONG.TXT":  {requested: "Long.txt"},
		"SHORT.TXT": {requested: "Short.txt"},
	}
	entries := []windowsLongAndShortName{
		{long: "Long.txt", short: ""},
		{long: "Alias.txt", short: "SHORT.TXT"},
		// A short name equal to the long name is not an alias claim.
		{long: "Long.txt", short: "Long.txt"},
	}
	if err := windowsRecordPublicEntryAuthorities(authorities, entries); err != nil {
		t.Fatal(err)
	}
	if got := authorities["LONG.TXT"]; got.longCount != 2 || got.aliasCount != 0 {
		t.Fatalf("long authority = %+v, want two long claims and no aliases", got)
	}
	if got := authorities["SHORT.TXT"]; got.longCount != 0 || got.aliasCount != 1 || got.aliasActual != "Alias.txt" {
		t.Fatalf("alias authority = %+v, want one long and one alias claim", got)
	}
}

func windowsDirectoryNamesBuffer(names ...string) []byte {
	entries := make([][]byte, len(names))
	total := 0
	for index, name := range names {
		nameBytes := len(utf16.Encode([]rune(name))) * 2
		size := (windowsFileNamesInformationHeader + nameBytes + 3) &^ 3
		entries[index] = make([]byte, size)
		binary.LittleEndian.PutUint32(entries[index][8:12], uint32(nameBytes))
		units := utf16.Encode([]rune(name))
		for unitIndex, unit := range units {
			binary.LittleEndian.PutUint16(entries[index][windowsFileNamesInformationHeader+unitIndex*2:], unit)
		}
		if index < len(names)-1 {
			binary.LittleEndian.PutUint32(entries[index][0:4], uint32(size))
		}
		total += size
	}
	buffer := make([]byte, 0, total)
	for _, entry := range entries {
		buffer = append(buffer, entry...)
	}
	return buffer
}

func windowsTestFileBothDirectoryBuffer(t *testing.T, longName, shortName string, next int) []byte {
	t.Helper()
	longUnits := utf16.Encode([]rune(longName))
	shortUnits := utf16.Encode([]rune(shortName))
	if len(shortUnits) > 12 {
		t.Fatalf("short test name has %d UTF-16 units", len(shortUnits))
	}
	minimum := windowsFileBothDirectoryInfoHeader + len(longUnits)*2
	length := minimum
	if next != 0 {
		length = next + windowsFileBothDirectoryInfoHeader + 2
	}
	words := make([]uint64, (length+7)/8)
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)
	info := (*windowsFileBothDirectoryInfo)(unsafe.Pointer(&buffer[0]))
	info.nextEntryOffset = uint32(next)
	info.fileNameLength = uint32(len(longUnits) * 2)
	info.shortNameLength = uint8(len(shortUnits) * 2)
	copy(info.shortName[:], shortUnits)
	copy(unsafe.Slice(&info.fileName[0], len(longUnits)), longUnits)
	return buffer
}
