//go:build linux || darwin

package output

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"golang.org/x/sys/unix"
)

func TestOpenChildFileRejectsNonRegularWithoutBlockingAndRestoresBlockingMode(t *testing.T) {
	dirPath := t.TempDir()
	root, _, err := OpenContainer(dirPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := unix.Mkfifo(filepath.Join(dirPath, "pipe"), uint32(privateFileMode)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dirPath, "subdir"), privateDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirPath, "target.txt"), []byte("ok"), privateFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(dirPath, "link")); err != nil {
		t.Fatal(err)
	}

	nonRegularCases := []struct {
		name  string
		entry string
		flags int
	}{
		{name: "fifo rdonly", entry: "pipe", flags: os.O_RDONLY},
		{name: "fifo wronly", entry: "pipe", flags: os.O_WRONLY},
		{name: "fifo rdwr", entry: "pipe", flags: os.O_RDWR},
		{name: "fifo rdwr trunc", entry: "pipe", flags: os.O_RDWR | os.O_CREATE | os.O_TRUNC},
		{name: "dir rdonly", entry: "subdir", flags: os.O_RDONLY},
		{name: "dir rdwr", entry: "subdir", flags: os.O_RDWR},
		{name: "symlink rdonly", entry: "link", flags: os.O_RDONLY},
	}
	for _, tc := range nonRegularCases {
		t.Run(tc.name, func(t *testing.T) {
			f, openErr := openChildFile(root, tc.entry, tc.flags, privateFileMode)
			if f != nil {
				_ = f.Close()
			}
			if !errors.Is(openErr, ErrUnsafeNamespace) {
				t.Fatalf("openChildFile(%q, 0x%x) = %v, want ErrUnsafeNamespace", tc.entry, tc.flags, openErr)
			}
		})
	}

	regularCases := []struct {
		name         string
		entry        string
		flags        int
		wantNonblock bool
	}{
		{name: "exclusive create", entry: "fresh.part", flags: os.O_RDWR | os.O_CREATE | os.O_EXCL, wantNonblock: false},
		{name: "existing rdonly", entry: "target.txt", flags: os.O_RDONLY, wantNonblock: false},
		{name: "existing rdwr", entry: "target.txt", flags: os.O_RDWR, wantNonblock: false},
		{name: "caller nonblock preserved", entry: "target.txt", flags: os.O_RDONLY | unix.O_NONBLOCK, wantNonblock: true},
	}
	for _, tc := range regularCases {
		t.Run(tc.name, func(t *testing.T) {
			f, openErr := openChildFile(root, tc.entry, tc.flags, privateFileMode)
			if openErr != nil {
				t.Fatalf("openChildFile(%q, 0x%x) failed: %v", tc.entry, tc.flags, openErr)
			}
			defer f.Close()

			gotFlags, fcntlErr := unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
			runtime.KeepAlive(f)
			if fcntlErr != nil {
				t.Fatalf("F_GETFL(%q): %v", tc.entry, fcntlErr)
			}
			gotNonblock := gotFlags&unix.O_NONBLOCK != 0
			if gotNonblock != tc.wantNonblock {
				t.Fatalf("O_NONBLOCK for %q = %v (flags=0x%x), want %v", tc.entry, gotNonblock, gotFlags, tc.wantNonblock)
			}
		})
	}
}

func TestFIFOInControlDirDoesNotDeadlockReceiveOrRecovery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ctrlPath := filepath.Join(root, ControlDirName)

	badOpID := testID16[receivecontract.OperationID](0xa1)
	badOpDir := filepath.Join(ctrlPath, hex.EncodeToString(badOpID.Bytes()))
	if err := os.MkdirAll(badOpDir, privateDirMode); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(badOpDir, journalFileName), uint32(privateFileMode)); err != nil {
		t.Fatal(err)
	}

	// An unrelated receive must isolate the FIFO-poisoned sibling header without
	// blocking while holding dirLock.
	auth := newBoundAuthority(t, root)
	sel := testSelectionSpec(t, 0xa2)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		_ = auth.Close()
		t.Fatalf("SelectOperation with FIFO sibling = (kind=%v, err=%v), want LookupCreateReady", lookup.Kind(), err)
	}
	art, err := receivecontract.NewSingleFileDirectoryTree(testID16[catalog.FileID](0xa3), "ok.bin", "ok.bin")
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		_ = auth.Close()
		t.Fatalf("CreateOperation with FIFO sibling = %v, want nil", err)
	}
	goodIntent, _ := op.ReceiveIntent()
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	// Recovery listing must not wedge on the FIFO journal and must still report
	// the healthy sibling alongside unknownEntries.
	inv, err := ListOperations(ctx, root)
	if err != nil {
		t.Fatalf("ListOperations with FIFO sibling = %v, want nil", err)
	}
	if inv.Status() != RecoveryListNeedsAttention || !inv.UnknownEntries() || len(inv.Summaries()) != 1 ||
		inv.Summaries()[0].OperationID() != goodIntent.OperationID() {
		t.Fatalf("ListOperations = %+v, want needs-attention with unknownEntries and healthy op %v", inv, goodIntent.OperationID())
	}

	// Explicit discard must be able to remove the FIFO-poisoned scratch dir.
	if _, err := DiscardOperation(ctx, root, badOpID); err != nil {
		t.Fatalf("DiscardOperation on FIFO sibling = %v, want nil", err)
	}
	if _, err := os.Lstat(badOpDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("badOpDir still exists after discard: %v", err)
	}
}

func TestFIFOAuthorityRefFailsFastAndReleasesDirLock(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ctrlPath := filepath.Join(root, ControlDirName)
	if err := os.MkdirAll(ctrlPath, privateDirMode); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(ctrlPath, authorityIDFile), uint32(privateFileMode)); err != nil {
		t.Fatal(err)
	}

	auth := newBoundAuthority(t, root)
	defer auth.Close()

	sel := testSelectionSpec(t, 0xb1)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("SelectOperation = (kind=%v, err=%v), want LookupCreateReady", lookup.Kind(), err)
	}
	art, err := receivecontract.NewSingleFileDirectoryTree(testID16[catalog.FileID](0xb2), "file.bin", "file.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.CreateOperation(ctx, lookup, art); !errors.Is(err, ErrUnsafeNamespace) {
		t.Fatalf("CreateOperation with FIFO authority.id = %v, want ErrUnsafeNamespace", err)
	}

	// Confirm dirLock was released cleanly by re-acquiring it on the container.
	rootDir, _, err := OpenContainer(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer rootDir.Close()
	lock, err := acquireDirLock(rootDir)
	if err != nil {
		t.Fatalf("acquireDirLock after FIFO authority.id failure: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
