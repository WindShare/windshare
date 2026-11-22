//go:build windows || linux || darwin

package osfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestOrdinaryResumeAdmitsReopenedMetadataToCurrentRevision(t *testing.T) {
	for name, metadata := range map[string]content.SourceMetadata{
		"metadata agreement": {1}, "metadata unavailable": {}, "metadata mismatch": {2},
	} {
		t.Run(name, func(t *testing.T) { testOrdinaryMetadataResume(t, metadata) })
	}
}

func testOrdinaryMetadataResume(t *testing.T, metadata content.SourceMetadata) {
	ctx := context.Background()
	root := t.TempDir()
	authority, operation, intent := openPublicOrdinaryOperation(t, root, 0x61)
	session, err := operation.OpenDirectTree(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	parent := admitRetainedCandidateRoot(t, session, intent, 0x62)
	file := nativeDirectTreeTestFile(t, session, intent, 0x63, "resume.bin", 8, parent)
	previous := content.BindRevisionMetadata(file.Descriptor(), content.SourceMetadata{1})
	file = nativeDirectTreeFileWithDescriptor(t, session, intent, previous, "resume.bin", parent)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatal("missing original transaction")
	}
	if err := tx.WriteRange(ctx, 0, []byte("part")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	var stagePath string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ".part" {
			if stagePath != "" {
				t.Fatal("more than one partial output")
			}
			stagePath = path
		}
		return nil
	})
	if err != nil || stagePath == "" {
		t.Fatalf("partial stage missing: %v", err)
	}
	stage, err := os.Stat(stagePath)
	if err != nil {
		t.Fatal(err)
	}

	stream, err := diagnostics.NewStream(256, nil)
	if err != nil {
		t.Fatal(err)
	}
	var records []diagnostics.Record
	drainRecords := func() {
		for {
			select {
			case record := <-stream.Records():
				records = append(records, record)
			default:
				return
			}
		}
	}
	reopen := func() {
		t.Helper()
		authority, err = NewOutputAuthority(OutputConfig{RootPath: root, Diagnostics: stream.Sink()})
		if err != nil {
			t.Fatal(err)
		}
		opened := authority
		t.Cleanup(func() { _ = opened.Close() })
		if err := authority.BindDestination(ctx); err != nil {
			t.Fatal(err)
		}
		lookup, err := authority.SelectOperation(ctx, intent.SelectionSpec(), receivecontract.AutomaticOperation())
		if err != nil || lookup.Kind() != OutputLookupReopened {
			t.Fatalf("reopen lookup = (%v, %v)", lookup.Kind(), err)
		}
		reopenedIntent, _ := lookup.Operation().ReceiveIntent()
		session, err = lookup.Operation().OpenDirectTree(ctx, reopenedIntent)
		if err != nil {
			t.Fatal(err)
		}
		parent = admitRetainedCandidateRoot(t, session, intent, 0x62)
	}
	reopen()
	currentRevision := previous.FileRevision()
	currentRevision[0]++
	current, err := content.NewFileRevisionDescriptor(previous.ShareInstance(), previous.FileID(), currentRevision, previous.Geometry(), previous.ModifiedTime())
	if err != nil {
		t.Fatal(err)
	}
	current = content.BindRevisionMetadata(current, metadata)
	file = nativeDirectTreeFileWithDescriptor(t, session, intent, current, "resume.bin", parent)
	start, err = session.BeginFile(ctx, file)
	drainRecords()
	wantPrev := diagnostics.Identity(previous.FileRevision().Bytes()).Text()
	wantCur := diagnostics.Identity(current.FileRevision().Bytes()).Text()
	if metadata != previous.SourceMetadata() {
		wantReason := content.ComparisonMetadataChanged
		if metadata.IsZero() {
			wantReason = content.ComparisonMetadataUnavailable
			want, _ := fault.NewSource(fault.ScopeFileLocal, fault.SourceUnavailable)
			got, _ := fault.NormalizeBoundaryError(err).Fault()
			if got != want {
				t.Fatalf("metadata unavailable returned %v, want %v", err, want)
			}
		} else {
			settlement, immediate := start.ImmediateSettlement()
			_, reason, blocked := settlement.ItemBlock()
			if err != nil || !immediate || !blocked || reason != transfer.ItemBlockRevisionConflict {
				t.Fatalf("metadata mismatch did not block the revision: %v", err)
			}
		}
		retained := false
		for _, record := range records {
			p := record.Payload()
			if p.Get("previous_revision").Text() == wantPrev && p.Get("current_revision").Text() == wantCur &&
				p.Get("retained_bytes").Uint() == 4 && p.Get("recovery_reason").Text() == string(wantReason) {
				retained = true
			}
		}
		payload, err := os.ReadFile(stagePath)
		if !retained || err != nil || len(payload) < 4 || string(payload[:4]) != "part" {
			t.Fatal("inapplicable source lost retained data or recovery evidence")
		}
		if _, err := session.PauseTree(ctx); err != nil {
			t.Fatal(err)
		}
		if !metadata.IsZero() {
			return
		}
		if err := authority.Close(); err != nil {
			t.Fatal(err)
		}
		reopen()

		currentRevision[0]++
		current, err = content.NewFileRevisionDescriptor(previous.ShareInstance(), previous.FileID(), currentRevision, previous.Geometry(), previous.ModifiedTime())
		if err != nil {
			t.Fatal(err)
		}
		current = content.BindRevisionMetadata(current, previous.SourceMetadata())
		file = nativeDirectTreeFileWithDescriptor(t, session, intent, current, "resume.bin", parent)
		start, err = session.BeginFile(ctx, file)
		drainRecords()
		wantCur = diagnostics.Identity(current.FileRevision().Bytes()).Text()
	}
	if err != nil {
		t.Fatal(err)
	}
	tx, progress, ok := start.Transaction()
	if !ok || len(progress.Ranges().Ranges()) != 1 || progress.Ranges().Ranges()[0].End != 4 {
		t.Fatal("reopened metadata lost the durable prefix")
	}
	reopened, err := os.Stat(stagePath)
	if err != nil || !os.SameFile(stage, reopened) {
		t.Fatalf("resume replaced the owned partial: %v", err)
	}
	found := false
	for _, record := range records {
		p := record.Payload()
		if p.Get("previous_revision").Text() == wantPrev && p.Get("current_revision").Text() == wantCur &&
			p.Get("reused_bytes").Uint() == 4 {
			found = true
		}
	}
	if !found {
		t.Fatal("metadata admission lacks correlated evidence")
	}
	if err := tx.WriteRange(ctx, 4, []byte("done")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := os.ReadFile(filepath.Join(nativeDirectTreeResultRoot(t, root, intent), "resume.bin"))
	if err != nil || string(result) != "partdone" {
		t.Fatalf("delivery=%q err=%v", result, err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
}
