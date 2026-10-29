//go:build windows

package outputportable

import (
	"bytes"
	"testing"

	"golang.org/x/sys/windows"
)

func TestDirectoryRecoveryIgnoresRelocatedWindowsFileIndex(t *testing.T) {
	before := windows.ByHandleFileInformation{VolumeSerialNumber: 17, FileIndexLow: 42, CreationTime: windows.NsecToFiletime(1_700_000_000_000_000_000)}
	after := before
	after.FileIndexHigh, after.FileIndexLow = 11, 99
	first, err := windowsCreationStamp(before)
	if err != nil {
		t.Fatal(err)
	}
	second, err := windowsCreationStamp(after)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("directory-entry relocation revoked persisted claim")
	}
	after.CreationTime.LowDateTime++
	changed, err := windowsCreationStamp(after)
	if err != nil || bytes.Equal(first, changed) {
		t.Fatal("changed creation metadata inherited the claim")
	}
	if _, err := windowsCreationStamp(windows.ByHandleFileInformation{}); err != nil {
		t.Fatal("volume root timestamp rejected")
	}
}
