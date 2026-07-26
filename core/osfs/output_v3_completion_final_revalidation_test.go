package osfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3CompletionRevalidatesPublishedFinalBeforeRetirement(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*testing.T, string)
		reason      resumestate.QuarantineReason
		verifyFinal func(*testing.T, string, []byte)
	}{
		{
			name: "deleted final",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
			reason: resumestate.QuarantineFinalMismatch,
			verifyFinal: func(t *testing.T, path string, _ []byte) {
				t.Helper()
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("completion recreated deleted final: %v", err)
				}
			},
		},
		{
			name: "replaced final",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			reason: resumestate.QuarantineFinalMismatch,
			verifyFinal: func(t *testing.T, path string, _ []byte) {
				t.Helper()
				actual, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(actual, []byte("foreign")) {
					t.Fatalf("completion changed replacement final = %q, %v", actual, err)
				}
			},
		},
		{
			name: "metadata drift",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				drifted := time.Unix(1_800_000_000, 0)
				if err := os.Chtimes(path, drifted, drifted); err != nil {
					t.Fatal(err)
				}
			},
			reason: resumestate.QuarantineMetadataMismatch,
			verifyFinal: func(t *testing.T, path string, payload []byte) {
				t.Helper()
				actual, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(actual, payload) {
					t.Fatalf("completion changed metadata-drifted final = %q, %v", actual, err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			payload := []byte("published")
			selection := v3RecoverySelection(t, true, uint64(len(payload)))
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
			file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
			transaction := v3RecoveryBeginTransaction(t, opened.Session, file)
			if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
				t.Fatal(err)
			}
			published, err := transaction.Commit(context.Background())
			if err != nil || published.Kind() != transfer.FilePublished {
				t.Fatalf("publish before completion race = (kind=%v, err=%v)", published.Kind(), err)
			}

			finalPath := filepath.Join(root, filepath.FromSlash(file.Path))
			test.mutate(t, finalPath)
			sessionID := opened.Session.SessionID()
			completed, err := opened.Session.CompleteJob(context.Background(), transfer.JobSucceeded)
			if err != nil || completed.Kind() != transfer.JobPausedNeedsAttention {
				t.Fatalf("complete changed published final = (kind=%v, err=%v), want needs attention", completed.Kind(), err)
			}
			test.verifyFinal(t, finalPath, payload)

			recordName := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(file.Path))
			recordPath := filepath.Join(
				v3RecoverySessionPath(root, selection, sessionID),
				resumestate.FilesDirectoryName, recordName.Shard(), recordName.Name(),
			)
			encoded, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatalf("read quarantined published record: %v", err)
			}
			record, err := resumestate.DecodeFileRecord(encoded)
			if err != nil || record.Phase() != resumestate.FileQuarantined || record.QuarantineReason() != test.reason {
				t.Fatalf("published completion record = (phase=%v, reason=%v, err=%v)",
					record.Phase(), record.QuarantineReason(), err)
			}
			anchorName := resumestate.AnchorName(record.OutputObject())
			anchorPath := filepath.Join(
				v3RecoverySessionPath(root, selection, sessionID),
				resumestate.AnchorsDirectoryName, anchorName.Shard(), anchorName.Name(),
			)
			if _, err := os.Stat(anchorPath); err != nil {
				t.Fatalf("completion removed last internal witness: %v", err)
			}
		})
	}
}

func TestOutputV3CompletionHoldsPublishedInternalCleanupAmbiguity(t *testing.T) {
	root := v3RecoveryRoot(t)
	payload := []byte("published")
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file)
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	published, err := transaction.Commit(context.Background())
	if err != nil || published.Kind() != transfer.FilePublished {
		t.Fatalf("initial publication = (kind=%v, err=%v)", published.Kind(), err)
	}
	record := outputV3PersistedFileRecord(t, opened.Session, file.Path)
	sessionID := opened.Session.SessionID()
	stage := resumestate.StageName(record.OutputObject())
	stagePath := filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID),
		resumestate.StagesDirectoryName, stage.Shard(), stage.Name(),
	)
	foreignStage := []byte("foreign-stage")
	if err := os.WriteFile(stagePath, foreignStage, 0o600); err != nil {
		t.Fatal(err)
	}

	completed, err := opened.Session.CompleteJob(context.Background(), transfer.JobSucceeded)
	if err != nil || completed.Kind() != transfer.JobPausedNeedsAttention {
		t.Fatalf("completion cleanup hold = (kind=%v, err=%v)", completed.Kind(), err)
	}
	recordName := resumestate.FileRecordName(record.LocatorDigest())
	recordPath := filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID),
		resumestate.FilesDirectoryName, recordName.Shard(), recordName.Name(),
	)
	encoded, err := os.ReadFile(recordPath)
	persisted, decodeErr := resumestate.DecodeFileRecord(encoded)
	if err != nil || decodeErr != nil || persisted.Phase() != resumestate.FilePublished ||
		persisted.QuarantineReason() != 0 {
		t.Fatalf("completion cleanup record = (phase=%v, quarantine=%v, read=%v, decode=%v)",
			persisted.Phase(), persisted.QuarantineReason(), err, decodeErr)
	}
	if actual, readErr := os.ReadFile(stagePath); readErr != nil || !bytes.Equal(actual, foreignStage) {
		t.Fatalf("completion changed foreign stage = %q, %v", actual, readErr)
	}
	finalPath := filepath.Join(root, filepath.FromSlash(file.Path))
	if actual, readErr := os.ReadFile(finalPath); readErr != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("completion changed final = %q, %v", actual, readErr)
	}
	anchor := resumestate.AnchorName(record.OutputObject())
	anchorPath := filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID),
		resumestate.AnchorsDirectoryName, anchor.Shard(), anchor.Name(),
	)
	if _, statErr := os.Stat(anchorPath); statErr != nil {
		t.Fatalf("completion removed published anchor: %v", statErr)
	}
}

func TestOutputV3CompletionHoldsRetiringInternalCleanupAmbiguity(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
	record := v3RecoveryPrepareRetiringCut(t, opened.Session, file)
	sessionID := opened.Session.SessionID()
	anchor := resumestate.AnchorName(record.OutputObject())
	anchorPath := filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID),
		resumestate.AnchorsDirectoryName, anchor.Shard(), anchor.Name(),
	)
	if err := os.Remove(anchorPath); err != nil {
		t.Fatal(err)
	}

	completed, err := opened.Session.CompleteJob(context.Background(), transfer.JobSucceeded)
	if err != nil || completed.Kind() != transfer.JobPausedNeedsAttention {
		t.Fatalf("retirement cleanup hold = (kind=%v, err=%v)", completed.Kind(), err)
	}
	recordName := resumestate.FileRecordName(record.LocatorDigest())
	recordPath := filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID),
		resumestate.FilesDirectoryName, recordName.Shard(), recordName.Name(),
	)
	encoded, err := os.ReadFile(recordPath)
	persisted, decodeErr := resumestate.DecodeFileRecord(encoded)
	if err != nil || decodeErr != nil || persisted.Phase() != resumestate.FileRetiring ||
		persisted.QuarantineReason() != 0 {
		t.Fatalf("retirement cleanup record = (phase=%v, quarantine=%v, read=%v, decode=%v)",
			persisted.Phase(), persisted.QuarantineReason(), err, decodeErr)
	}
	stage := resumestate.StageName(record.OutputObject())
	stagePath := filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID),
		resumestate.StagesDirectoryName, stage.Shard(), stage.Name(),
	)
	if _, statErr := os.Stat(stagePath); statErr != nil {
		t.Fatalf("completion removed retiring stage: %v", statErr)
	}
}
