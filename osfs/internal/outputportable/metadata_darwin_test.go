//go:build darwin

package outputportable

import (
	"bytes"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDirectoryRecoveryIgnoresRelocatedDarwinInode(t *testing.T) {
	before := unix.Stat_t{Dev: 1, Ino: 42, Btim: unix.Timespec{Sec: 1700000000}}
	after := before
	after.Dev, after.Ino = 2, 99
	first, err := darwinCreationStamp(before)
	if err != nil {
		t.Fatal(err)
	}
	second, err := darwinCreationStamp(after)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("mount or directory-entry relocation revoked claim")
	}
	after.Btim.Sec++
	changed, err := darwinCreationStamp(after)
	if err != nil || bytes.Equal(first, changed) {
		t.Fatal("changed creation metadata inherited the claim")
	}
}
