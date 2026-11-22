//go:build windows || linux || darwin

package output

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestReadJournalHeaderReadsOnlyImmutableFirstRecord(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, op, intent := openActiveSingleFileOp(t, root, 0xd1)
	opID := intent.OperationID()

	// Append a lifecycle update and force compaction so line 1 is rewritten
	// through compactLocked, then verify the immutable header remains intact.
	if err := op.RecordShareStopped(ctx); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	op.journal.mu.Lock()
	compactErr := op.journal.compactLocked()
	op.journal.mu.Unlock()
	if compactErr != nil {
		_ = auth.Close()
		t.Fatal(compactErr)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	opPath := filepath.Join(root, ControlDirName, hex.EncodeToString(opID.Bytes()))
	journalPath := filepath.Join(opPath, journalFileName)

	// Append non-tail corrupt lines that cause full readJournalState to fail.
	f, err := os.OpenFile(journalPath, os.O_WRONLY|os.O_APPEND, privateFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("deadbeef {\"k\":\"partial\",\"corrupt\":true}\nstill-corrupt\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	opDir, err := openDirHandle(opPath)
	if err != nil {
		t.Fatal(err)
	}
	defer opDir.Close()

	if _, _, err := readJournalState(opDir); !errors.Is(err, errJournalCorrupt) {
		t.Fatalf("readJournalState on corrupt body = %v, want errJournalCorrupt", err)
	}
	header, err := readJournalHeader(opDir)
	if err != nil {
		t.Fatalf("readJournalHeader = %v, want nil", err)
	}
	if !header.Intent.Equal(intent) ||
		header.SelectionDigest != intent.SelectionSpec().Digest() ||
		header.Redownload {
		t.Fatalf("readJournalHeader mismatch: got %+v, want intent=%x redownload=false", header, intent.Digest())
	}

	// Leading blank lines before a valid meta header remain tolerated.
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, append([]byte("\n\n"), raw...), privateFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournalHeader(opDir); err != nil {
		t.Fatalf("readJournalHeader with leading blank lines = %v", err)
	}
}

func TestReadJournalHeaderRejectsMalformedInputs(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		dir, err := openDirHandle(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer dir.Close()
		if _, err := readJournalHeader(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing journal = %v, want fs.ErrNotExist", err)
		}
	})

	cases := []struct {
		name    string
		content []byte
	}{
		{name: "empty file", content: nil},
		{name: "blank lines only", content: []byte("\n\n")},
		{name: "bad crc", content: []byte("00000000 {\"k\":\"meta\"}\n")},
		{name: "non-meta first line", content: mustEncodeJournalLine(t, journalRecord{
			Kind: "retired", Path: "a.bin",
		})},
		{name: "lifecycle meta without intent", content: mustEncodeJournalLine(t, journalRecord{
			Kind: "meta", Lifecycle: journalLifecycleStopped, StateGen: 2,
		})},
		{name: "oversized line without newline", content: bytes.Repeat([]byte("a"), journalMaxLineBytes+64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir()
			if err := os.WriteFile(filepath.Join(path, journalFileName), tc.content, privateFileMode); err != nil {
				t.Fatal(err)
			}
			dir, err := openDirHandle(path)
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			if _, err := readJournalHeader(dir); !errors.Is(err, errJournalCorrupt) {
				t.Fatalf("readJournalHeader(%s) = %v, want errJournalCorrupt", tc.name, err)
			}
		})
	}
}

func TestAutomaticLookupAndSingleFileReservationUseHeaderOnly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// Create a paused single-file task A claiming retirementFileName, then
	// corrupt its journal body (leaving line 1 intact). If lookup or single-file
	// reservation replayed the full journal, both would fail to see task A.
	selA, intentA, _ := createPausedSingleFileOp(t, root, 0xe1)
	journalA := filepath.Join(root, ControlDirName, hex.EncodeToString(intentA.OperationID().Bytes()), journalFileName)
	f, err := os.OpenFile(journalA, os.O_WRONLY|os.O_APPEND, privateFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("deadbeef {\"k\":\"delivered\",\"corrupt\":true}\nsecond-corrupt-line\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Also add an unrelated redownload single-file op claiming a distinct name
	// to verify redownload tasks stay excluded from automatic matching while
	// still protecting their reserved top-level filename.
	redlAuth, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := redlAuth.BindDestination(ctx); err != nil {
		_ = redlAuth.Close()
		t.Fatal(err)
	}
	selB := testSelectionSpec(t, 0xe7)
	redlLookup, err := redlAuth.SelectOperation(ctx, selB, receivecontract.RedownloadOperation())
	if err != nil || redlLookup.Kind() != LookupCreateReady {
		_ = redlAuth.Close()
		t.Fatalf("redownload select = (%v, %v)", redlLookup.Kind(), err)
	}
	redlArt, err := receivecontract.NewSingleFileDirectoryTree(testID16[catalog.FileID](0xe9), retirementFileName, retirementFileName)
	if err != nil {
		_ = redlAuth.Close()
		t.Fatal(err)
	}
	redlOp, err := redlAuth.CreateOperation(ctx, redlLookup, redlArt)
	if err != nil {
		_ = redlAuth.Close()
		t.Fatal(err)
	}
	redlIntent, _ := redlOp.ReceiveIntent()
	redlRes, _ := redlIntent.MaterializationPlan().DestinationReservation()
	if redlRes.PhysicalName() == retirementFileName {
		_ = redlAuth.Close()
		t.Fatalf("redownload single-file op collided with task A's reserved name %q", retirementFileName)
	}
	if err := redlAuth.Close(); err != nil {
		t.Fatal(err)
	}

	// Start an unrelated automatic single-file download with the same suggested
	// name: it must neither match selB's redownload task nor collide with task
	// A's or the redownload task's reserved names.
	autoAuth, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer autoAuth.Close()
	if err := autoAuth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookupB, err := autoAuth.SelectOperation(ctx, selB, receivecontract.AutomaticOperation())
	if err != nil || lookupB.Kind() != LookupCreateReady {
		t.Fatalf("unrelated automatic select = (%v, %v), want LookupCreateReady", lookupB.Kind(), err)
	}
	opB, err := autoAuth.CreateOperation(ctx, lookupB, redlArt)
	if err != nil {
		t.Fatalf("unrelated single-file create = %v", err)
	}
	intentB, _ := opB.ReceiveIntent()
	resB, _ := intentB.MaterializationPlan().DestinationReservation()
	if resB.PhysicalName() == retirementFileName || resB.PhysicalName() == redlRes.PhysicalName() {
		t.Fatalf("unrelated single-file op collided with existing claims: got %q, existing {%q, %q}",
			resB.PhysicalName(), retirementFileName, redlRes.PhysicalName())
	}

	// Reopening selA matches task A from its header and defers full replay to
	// openTaskJournal, surfacing LookupNeedsAttention instead of silently
	// ignoring the damaged task and creating a duplicate.
	reopenA, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopenA.Close()
	if err := reopenA.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookupA, err := reopenA.SelectOperation(ctx, selA, receivecontract.AutomaticOperation())
	if err != nil || lookupA.Kind() != LookupNeedsAttention || lookupA.Attention() != AttentionOperation {
		t.Fatalf("reopen damaged task A = (kind=%v, attn=%q, err=%v), want LookupNeedsAttention(%q)",
			lookupA.Kind(), lookupA.Attention(), err, AttentionOperation)
	}
}

func mustEncodeJournalLine(t *testing.T, rec journalRecord) []byte {
	t.Helper()
	line, err := encodeJournalLine(rec)
	if err != nil {
		t.Fatal(err)
	}
	return line
}
