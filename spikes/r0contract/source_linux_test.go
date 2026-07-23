//go:build linux

package r0contract

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type posixMutationToken struct {
	device      uint64
	inode       uint64
	size        int64
	modifiedSec int64
	modifiedNS  int64
	changedSec  int64
	changedNS   int64
}

func TestLinuxMutationTokenDetectsWriteAndPathReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(path, []byte("first revision"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	original := linuxToken(t, source)

	if err := os.WriteFile(path, []byte("other revision"), 0o600); err != nil {
		t.Fatal(err)
	}
	forcedTime := time.Unix(original.modifiedSec+2, original.modifiedNS)
	if err := os.Chtimes(path, forcedTime, forcedTime); err != nil {
		t.Fatal(err)
	}
	if afterWrite := linuxToken(t, source); afterWrite == original {
		t.Fatal("same-object write did not change the mutation token")
	}

	moved := path + ".moved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement obj"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if linuxToken(t, source).inode == linuxToken(t, replacement).inode {
		t.Fatal("path replacement retained the source inode")
	}
}

func linuxToken(t *testing.T, file *os.File) posixMutationToken {
	t.Helper()
	information, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := information.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("FileInfo lacks syscall.Stat_t")
	}
	return posixMutationToken{
		device: uint64(stat.Dev), inode: stat.Ino, size: stat.Size,
		modifiedSec: stat.Mtim.Sec, modifiedNS: stat.Mtim.Nsec,
		changedSec: stat.Ctim.Sec, changedNS: stat.Ctim.Nsec,
	}
}
