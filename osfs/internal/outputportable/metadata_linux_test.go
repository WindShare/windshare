//go:build linux

package outputportable

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

func TestDirectoryRecoveryIgnoresRemountedLinuxDeviceAndInode(t *testing.T) {
	before := unix.Statx_t{Mask: unix.STATX_BTIME, Ino: 42, Dev_major: 8, Dev_minor: 1,
		Btime: unix.StatxTimestamp{Sec: 1700000000, Nsec: 10000000}}
	after := before
	after.Ino, after.Dev_minor = 99, 2
	first, err := linuxCreationStamp(before)
	if err != nil {
		t.Fatal(err)
	}
	second, err := linuxCreationStamp(after)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("remount revoked directory claim")
	}
	after.Btime.Sec++
	changed, err := linuxCreationStamp(after)
	if err != nil || bytes.Equal(first, changed) {
		t.Fatal("changed creation metadata inherited the claim")
	}
	if _, err := linuxCreationStamp(unix.Statx_t{}); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatal("missing creation evidence silently became native inode identity")
	}
}
