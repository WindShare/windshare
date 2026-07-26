//go:build windows

package osfs

import (
	"testing"
	"unicode/utf16"
	"unsafe"
)

func TestWindowsV3ParseLongAndShortNamesUsesNativeFILEBOTHLayout(t *testing.T) {
	buffer := windowsV3TestFileBothDirectoryBuffer(t, ".windshare-output", "WINDSH~1", 0)
	entries, err := windowsV3ParseLongAndShortNames(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].long != ".windshare-output" || entries[0].short != "WINDSH~1" {
		t.Fatalf("parsed long/short names = %+v", entries)
	}
}

func TestWindowsV3ParseLongAndShortNamesRejectsMisalignedNextEntry(t *testing.T) {
	if windowsV3FileBothDirectoryInfoAlign <= 4 {
		t.Skip("native FILE_BOTH layout requires only four-byte alignment")
	}
	const fourButNotEightAlignedOffset = 100
	buffer := windowsV3TestFileBothDirectoryBuffer(t, "a", "", fourButNotEightAlignedOffset)
	if _, err := windowsV3ParseLongAndShortNames(buffer); err == nil {
		t.Fatal("parser accepted a next-entry offset that misaligns the native structure")
	}
}

func windowsV3TestFileBothDirectoryBuffer(t *testing.T, longName, shortName string, next int) []byte {
	t.Helper()
	longUnits := utf16.Encode([]rune(longName))
	shortUnits := utf16.Encode([]rune(shortName))
	if len(shortUnits) > 12 {
		t.Fatalf("short test name has %d UTF-16 units", len(shortUnits))
	}
	minimum := windowsV3FileBothDirectoryInfoHeader + len(longUnits)*2
	length := minimum
	if next != 0 {
		length = next + windowsV3FileBothDirectoryInfoHeader + 2
	}
	words := make([]uint64, (length+7)/8)
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)
	info := (*windowsV3FileBothDirectoryInfo)(unsafe.Pointer(&buffer[0]))
	info.nextEntryOffset = uint32(next)
	info.fileNameLength = uint32(len(longUnits) * 2)
	info.shortNameLength = uint8(len(shortUnits) * 2)
	copy(info.shortName[:], shortUnits)
	copy(unsafe.Slice(&info.fileName[0], len(longUnits)), longUnits)
	return buffer
}
