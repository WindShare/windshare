//go:build windows || linux || darwin

package osfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/core/transfer"
)

func TestPortableResumeKeepsDeliveryAfterPublicFileMoves(t *testing.T) {
	for _, action := range []string{"move", "delete", "replace"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			a, op, intent := openPublicOrdinaryOperation(t, root, 0x31)
			s, err := a.OpenOperation(ctx, op)
			if err != nil {
				t.Fatal(err)
			}
			admission := admitRetainedCandidateRoot(t, s, intent, 0x32)
			f := nativeDirectTreeTestFile(t, s, intent, 0x33, "done.bin", 4, admission)
			start, err := s.BeginFile(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			tx, _, _ := start.Transaction()
			if err := tx.WriteRange(ctx, 0, []byte("done")); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PauseTree(ctx, transfer.JobPauseShutdown); err != nil {
				t.Fatal(err)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			final := filepath.Join(nativeDirectTreeResultRoot(t, root, intent), "done.bin")
			if action == "move" {
				err = os.Rename(final, filepath.Join(root, "moved.bin"))
			} else {
				err = os.Remove(final)
			}
			if err != nil {
				t.Fatal(err)
			}
			if action == "replace" {
				if err := os.WriteFile(final, []byte("user"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			a, err = NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			if _, err := a.BindDestination(ctx); err != nil {
				t.Fatal(err)
			}
			lookup, err := a.LookupActive(ctx, intent.SelectionSpec())
			if err != nil {
				t.Fatal(err)
			}
			s, err = a.OpenOperation(ctx, lookup.Operation())
			if err != nil {
				t.Fatal(err)
			}
			history, ok := s.(transfer.DeliveryHistory)
			if !ok {
				t.Fatal("missing delivery history")
			}
			delivery, found, err := history.LookupDelivery(ctx, f.Descriptor().FileID(), f.MaterializationRelativePath().String())
			if err != nil || !found || delivery.Size != 4 {
				t.Fatalf("receipt lost: %+v %v %v", delivery, found, err)
			}
			if _, err := s.PauseTree(ctx, transfer.JobPauseShutdown); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPortableResumeAcceptsMtimeDriftAndFlushesPause(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a, op, intent := openPublicOrdinaryOperation(t, root, 0x51)
	s, err := a.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	admission := admitRetainedCandidateRoot(t, s, intent, 0x52)
	f := nativeDirectTreeTestFile(t, s, intent, 0x53, "partial.bin", 8, admission)
	start, err := s.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, _ := start.Transaction()
	if err := tx.WriteRange(ctx, 0, []byte("part")); err != nil {
		t.Fatal(err)
	}
	accepted, err := tx.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	persisted, _ := accepted.Persisted()
	if accepted.Accepted().Ranges().IsEmpty() || !persisted.Ranges().IsEmpty() {
		t.Fatal("accepted progress was confused with persistence")
	}
	if _, err := tx.Pause(ctx, transfer.FilePauseShutdown); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PauseTree(ctx, transfer.JobPauseShutdown); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	stages := 0
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ".stage" {
			stages++
			return os.Chtimes(path, time.Now(), time.Now().Add(time.Hour))
		}
		return nil
	})
	if err != nil || stages != 1 {
		t.Fatalf("stages=%d %v", stages, err)
	}
	a, err = NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err := a.LookupActive(ctx, intent.SelectionSpec())
	if err != nil {
		t.Fatal(err)
	}
	s, err = a.OpenOperation(ctx, lookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	admission = admitRetainedCandidateRoot(t, s, intent, 0x52)
	f = nativeDirectTreeTestFile(t, s, intent, 0x53, "partial.bin", 8, admission)
	start, err = s.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	tx, ranges, ok := start.Transaction()
	if !ok || len(ranges.Ranges().Ranges()) != 1 || ranges.Ranges().Ranges()[0].End != 4 {
		t.Fatal("mtime drift discarded persisted ranges")
	}
	if err := tx.WriteRange(ctx, 4, []byte("done")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PauseTree(ctx, transfer.JobPauseShutdown); err != nil {
		t.Fatal(err)
	}
}
