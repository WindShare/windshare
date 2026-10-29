//go:build linux

package osfs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
)

// The CI harness unmounts a disposable FAT/exFAT image between the two processes.
// Ordinary short tests skip this test rather than requiring mount privileges.
func TestPortableFilesystemRemount(t *testing.T) {
	root := os.Getenv("WINDSHARE_REMOUNT_ROOT")
	if root == "" {
		t.Skip("requires the isolated filesystem image harness")
	}
	phase := os.Getenv("WINDSHARE_REMOUNT_PHASE")
	switch phase {
	case "create":
		for _, action := range []string{"resume", "discard"} {
			path := filepath.Join(root, action)
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("image fixture already exists")
			}
			pausedPortableFileAt(t, path, 8)
		}
		identity := remountIdentity(t, onlyPortableStage(t, filepath.Join(root, "resume")))
		encoded, err := json.Marshal(identity)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "identity.json"), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	case "recover":
		var previous [2]uint64
		encoded, err := os.ReadFile(filepath.Join(root, "identity.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &previous); err != nil {
			t.Fatal(err)
		}
		current := remountIdentity(t, onlyPortableStage(t, filepath.Join(root, "resume")))
		t.Logf("native identity before remount=%v after remount=%v", previous, current)
		if previous == current {
			t.Fatal("remount did not exercise changed native identity")
		}
		for _, action := range []string{"resume", "discard"} {
			t.Run(action, func(t *testing.T) { recoverRemountedOperation(t, filepath.Join(root, action), action) })
		}
	default:
		t.Fatalf("unknown remount test phase %q", phase)
	}
}

func remountIdentity(t *testing.T, path string) [2]uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	return [2]uint64{stat.Dev, stat.Ino}
}

func recoverRemountedOperation(t *testing.T, root, action string) {
	t.Helper()
	ctx := context.Background()
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	mode, err := authority.BindDestination(ctx)
	if err != nil || !mode.Resumable() {
		t.Fatalf("remount recovery admission: %+v %v", mode, err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(coverageC6Identity[catalog.ShareInstance](0x61),
		coverageC6Identity[catalog.DirectoryID](0x62), rules)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(ctx, selection)
	if err != nil || lookup.Kind() != FilesystemOutputLookupReopened {
		t.Fatalf("lost operation after remount: %v", err)
	}
	intent, ok := lookup.Operation().ReceiveIntent()
	if !ok {
		t.Fatal("retained operation lost its intent")
	}
	if action == "discard" {
		stage := onlyPortableStage(t, root)
		if err := authority.Close(); err != nil {
			t.Fatal(err)
		}
		recovery, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: root})
		if err != nil {
			t.Fatal(err)
		}
		result, err := recovery.Discard(ctx, intent.OperationID())
		if err != nil || result.State() != ResumeOperationDiscarded {
			t.Fatalf("remount discard: %+v %v", result, err)
		}
		if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned partial remains: %v", err)
		}
		return
	}
	session, err := authority.OpenOperation(ctx, lookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	admission := admitRetainedCandidateRoot(t, session, intent, 0x62)
	file := nativeDirectTreeTestFile(t, session, intent, 0x63, "file.bin", 8, admission)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	tx, ranges, ok := start.Transaction()
	if !ok || len(ranges.Ranges().Ranges()) != 1 || ranges.Ranges().Ranges()[0].End != 4 {
		t.Fatal("remount lost persisted ranges")
	}
	if err := tx.WriteRange(ctx, 4, []byte("done")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(ctx, transfer.JobPauseShutdown); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(nativeDirectTreeResultRoot(t, root, intent), "file.bin"))
	if err != nil || string(actual) != "datadone" {
		t.Fatalf("published payload: %q %v", actual, err)
	}
}
