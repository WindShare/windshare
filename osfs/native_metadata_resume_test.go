//go:build windows || linux || darwin

package osfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
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
	session, err := authority.OpenOperation(ctx, operation)
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
	if _, err := tx.Pause(ctx, transfer.FilePauseShutdown); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(ctx, transfer.JobPauseShutdown); err != nil {
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
		if !entry.IsDir() && filepath.Ext(path) == ".stage" {
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

	var events []FilesystemOutputTrace
	authority, err = NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root,
		Tracer: FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) { events = append(events, event) }),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	if _, err := authority.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(ctx, intent.SelectionSpec())
	if err != nil {
		t.Fatal(err)
	}
	session, err = authority.OpenOperation(ctx, lookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	parent = admitRetainedCandidateRoot(t, session, intent, 0x62)
	currentRevision := previous.FileRevision()
	currentRevision[0]++
	current, err := content.NewFileRevisionDescriptor(previous.ShareInstance(), previous.FileID(), currentRevision, previous.Geometry(), previous.ModifiedTime())
	if err != nil {
		t.Fatal(err)
	}
	current = content.BindRevisionMetadata(current, metadata)
	file = nativeDirectTreeFileWithDescriptor(t, session, intent, current, "resume.bin", parent)
	start, err = session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if metadata != previous.SourceMetadata() {
		settlement, immediate := start.ImmediateSettlement()
		_, reason, blocked := settlement.ItemBlock()
		if !immediate || !blocked || reason != transfer.ItemBlockRevisionConflict {
			t.Fatal("inapplicable source granted receiving authority")
		}
		wantReason := content.ComparisonMetadataChanged
		if metadata.IsZero() {
			wantReason = content.ComparisonMetadataUnavailable
		}
		retained := false
		for _, event := range events {
			if event.PreviousRevision == previous.FileRevision() && event.CurrentRevision == current.FileRevision() &&
				event.RetainedBytes == 4 && event.RecoveryReason == wantReason {
				retained = true
			}
		}
		payload, err := os.ReadFile(stagePath)
		if !retained || err != nil || len(payload) < 4 || string(payload[:4]) != "part" {
			t.Fatal("inapplicable source lost retained data or recovery evidence")
		}
		if _, err := session.PauseTree(ctx, transfer.JobPauseShutdown); err != nil {
			t.Fatal(err)
		}
		return
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
	for _, event := range events {
		if event.PreviousRevision == previous.FileRevision() && event.CurrentRevision == current.FileRevision() &&
			event.ReusedBytes == 4 {
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
	if _, err := session.PauseTree(ctx, transfer.JobPauseShutdown); err != nil {
		t.Fatal(err)
	}
}
