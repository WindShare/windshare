package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3RemovesRecognizedInterruptedStateUpdatesBeforeResumingContent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		written int
	}{
		{name: "created"},
		{name: "partial", written: 3},
		{name: "complete", written: 16},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, true, 1)
			sessionIDs := &v3RecoverySessionIDs{}
			authority := v3RecoveryAuthority(t, root, sessionIDs)
			opened := v3RecoveryOpen(t, authority, root, selection)
			file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
			transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*FileTransaction)
			record := transaction.resumable.Bound().Record()
			nonce, err := resumestate.UpdateNonceFromBytes(bytes.Repeat([]byte{0x63}, resumestate.UpdateNonceBytes))
			if err != nil {
				t.Fatal(err)
			}
			temporaryName := resumestate.UpdateTemporaryName(record.LocatorDigest(), nonce)
			if temporaryName.Shard() != resumestate.FileRecordName(record.LocatorDigest()).Shard() {
				t.Fatal("update temporary escaped its file-state shard")
			}
			temporary, err := transaction.recordDir.CreateFile(temporaryName.Name(), true, 16)
			if err != nil {
				t.Fatal(err)
			}
			if test.written != 0 {
				payload := bytes.Repeat([]byte{0x7a}, test.written)
				actual, err := temporary.WriteAt(payload, 0)
				if err != nil || actual != test.written {
					t.Fatalf("write temporary = (%d, %v), want %d", actual, err, test.written)
				}
			}
			if err := temporary.Close(); err != nil {
				t.Fatal(err)
			}
			transaction.lifecycle = FileTransactionClosed
			if err := transaction.closeHandles(); err != nil {
				t.Fatal(err)
			}
			opened.Session.finishFile(record.LocatorDigest(), transaction)
			sessionID := opened.Session.SessionID()
			v3RecoveryCloseSession(t, opened.Session)

			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			start, err := reopened.Session.BeginFile(
				context.Background(), v3RecoveryOutputFile(t, reopened.Session, selection, 1),
			)
			if err != nil {
				t.Fatal(err)
			}
			resumed, _, ok := start.Transaction()
			if !ok {
				t.Fatal("recognized update temporary prevented content resume")
			}
			temporaryPath := filepath.Join(
				v3RecoverySessionPath(root, selection, sessionID), resumestate.FilesDirectoryName,
				temporaryName.Shard(), temporaryName.Name(),
			)
			if _, err := os.Stat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("update temporary stat error = %v, want not exist", err)
			}
			if _, err := resumed.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip); err != nil {
				t.Fatal(err)
			}
			v3RecoveryCloseSession(t, reopened.Session)
		})
	}
}

func TestOutputV3DirectPublicationCollisionIsPublishBlocked(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	payload := []byte("owned output")
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	authority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, authority, root, selection)
	file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file)
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("foreign file")
	if len(foreign) != len(payload) {
		t.Fatal("test payloads must have equal size")
	}
	if err := os.WriteFile(filepath.Join(root, v3RecoveryFilePath), foreign, 0o600); err != nil {
		t.Fatal(err)
	}

	settlement, err := transaction.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Kind() != transfer.FilePublishBlocked {
		t.Fatalf("direct EEXIST settlement = %v, want %v", settlement.Kind(), transfer.FilePublishBlocked)
	}
	actual, err := os.ReadFile(filepath.Join(root, v3RecoveryFilePath))
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(foreign) {
		t.Fatalf("foreign final changed to %q", actual)
	}
	v3RecoveryCloseSession(t, opened.Session)
}

func TestOutputV3RecoveredPublishingAdoptsOnlyMatchingFinal(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		final      string
		settlement transfer.FileSettlementKind
	}{
		{name: "final-missing", final: "missing", settlement: transfer.FilePublished},
		{name: "matching-final", final: "matching", settlement: transfer.FilePublished},
		{name: "foreign-final", final: "foreign", settlement: transfer.FileQuarantined},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			payload := []byte("publication")
			selection := v3RecoverySelection(t, true, uint64(len(payload)))
			sessionIDs := &v3RecoverySessionIDs{}
			authority := v3RecoveryAuthority(t, root, sessionIDs)
			opened := v3RecoveryOpen(t, authority, root, selection)
			file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
			record := v3RecoveryPreparePublishingCut(t, opened.Session, file, payload, test.final)
			v3RecoveryCloseSession(t, opened.Session)

			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			recoveryFile := v3RecoveryOutputFile(t, reopened.Session, selection, uint64(len(payload)))
			start, err := reopened.Session.BeginFile(context.Background(), recoveryFile)
			if err != nil {
				t.Fatal(err)
			}
			settlement, ok := start.ImmediateSettlement()
			if !ok || settlement.Kind() != test.settlement {
				t.Fatalf("publishing recovery = (%v, %v), want %v", settlement.Kind(), ok, test.settlement)
			}
			if test.final != "foreign" {
				actual, err := os.ReadFile(filepath.Join(root, v3RecoveryFilePath))
				if err != nil {
					t.Fatal(err)
				}
				if string(actual) != string(payload) {
					t.Fatalf("published bytes = %q, want %q", actual, payload)
				}
				stage := resumestate.StageName(record.OutputObject())
				stagePath := filepath.Join(
					v3RecoverySessionPath(root, selection, reopened.Session.SessionID()),
					resumestate.StagesDirectoryName, stage.Shard(), stage.Name(),
				)
				if _, err := os.Stat(stagePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("published stage stat error = %v, want not exist", err)
				}
			}
			v3RecoveryCloseSession(t, reopened.Session)
		})
	}
}

func TestOutputV3RecoversEveryRetirementRemovalCutInOrder(t *testing.T) {
	t.Parallel()
	for removed := 0; removed <= 3; removed++ {
		t.Run(outputV3RetirementCutLabel(removed), func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, true, 1)
			sessionIDs := &v3RecoverySessionIDs{}
			authority := v3RecoveryAuthority(t, root, sessionIDs)
			opened := v3RecoveryOpen(t, authority, root, selection)
			file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
			record := v3RecoveryPrepareRetiringCut(t, opened.Session, file)
			sessionID := opened.Session.SessionID()
			v3RecoveryCloseSession(t, opened.Session)

			stage := resumestate.StageName(record.OutputObject())
			anchor := resumestate.AnchorName(record.OutputObject())
			state := resumestate.FileRecordName(record.LocatorDigest())
			sessionPath := v3RecoverySessionPath(root, selection, sessionID)
			removalOrder := []string{
				filepath.Join(sessionPath, resumestate.StagesDirectoryName, stage.Shard(), stage.Name()),
				filepath.Join(sessionPath, resumestate.AnchorsDirectoryName, anchor.Shard(), anchor.Name()),
				filepath.Join(sessionPath, resumestate.FilesDirectoryName, state.Shard(), state.Name()),
			}
			for _, path := range removalOrder[:removed] {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}

			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			recoveryFile := v3RecoveryOutputFile(t, reopened.Session, selection, 1)
			start, err := reopened.Session.BeginFile(context.Background(), recoveryFile)
			if err != nil {
				t.Fatal(err)
			}
			if removed < len(removalOrder) {
				settlement, ok := start.ImmediateSettlement()
				if !ok || settlement.Kind() != transfer.FileRetired {
					t.Fatalf("retirement recovery = (%v, %v), want one immediate FileRetired", settlement.Kind(), ok)
				}
				next, err := reopened.Session.BeginFile(context.Background(), recoveryFile)
				if err != nil {
					t.Fatal(err)
				}
				transaction, _, ok := next.Transaction()
				if !ok {
					t.Fatal("completed retirement produced a second immediate settlement")
				}
				if _, err := transaction.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip); err != nil {
					t.Fatal(err)
				}
			} else {
				transaction, _, ok := start.Transaction()
				if !ok {
					t.Fatal("fully retired record did not begin a fresh content transaction")
				}
				if _, err := transaction.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range removalOrder {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("retired private link %q stat error = %v, want not exist", path, err)
				}
			}
			v3RecoveryCloseSession(t, reopened.Session)
		})
	}
}

func v3RecoveryBeginTransaction(
	t *testing.T,
	session *Session,
	file transfer.OutputFile,
) transfer.FileTransaction {
	t.Helper()
	start, err := session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatal("BeginFile returned an immediate settlement")
	}
	return transaction
}

func v3RecoveryPreparePublishingCut(
	t *testing.T,
	session *Session,
	file transfer.OutputFile,
	payload []byte,
	finalState string,
) resumestate.FileRecord {
	t.Helper()
	transaction := v3RecoveryBeginTransaction(t, session, file).(*FileTransaction)
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.data.SetModifiedTime(transaction.descriptor.ModifiedTime()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.data.Sync(); err != nil {
		t.Fatal(err)
	}
	publishing, err := resumestate.PreparePublication(transaction.resumable)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.installFileRecord(
		transaction.recordDir, transaction.recordName, transaction.resumable.Bound(), publishing,
	); err != nil {
		t.Fatal(err)
	}
	transaction.resumable, err = resumestate.BindResumableFile(publishing, transaction.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	switch finalState {
	case "missing":
	case "matching":
		result, err := session.linkFinalNoReplace(publishing, v3TransactionPublicationWitness(transaction))
		if err != nil || result != resumestate.PublishLinkCreated {
			t.Fatalf("install matching final = (result=%v, err=%v)", result, err)
		}
	case "foreign":
		foreign := append([]byte(nil), payload...)
		foreign[0] ^= 0xff
		if err := os.WriteFile(filepath.Join(session.owner.rootPath, v3RecoveryFilePath), foreign, 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported final state %q", finalState)
	}
	record := publishing.Record()
	transaction.lifecycle = FileTransactionClosed
	if err := transaction.closeHandles(); err != nil {
		t.Fatal(err)
	}
	session.finishFile(record.LocatorDigest(), transaction)
	return record
}

func v3RecoveryPrepareRetiringCut(
	t *testing.T,
	session *Session,
	file transfer.OutputFile,
) resumestate.FileRecord {
	t.Helper()
	transaction := v3RecoveryBeginTransaction(t, session, file).(*FileTransaction)
	retiring, err := resumestate.PrepareIsolatedRetirement(transaction.resumable.Bound())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.installFileRecord(
		transaction.recordDir, transaction.recordName, transaction.resumable.Bound(), retiring,
	); err != nil {
		t.Fatal(err)
	}
	record := retiring.Record()
	transaction.lifecycle = FileTransactionClosed
	if err := transaction.closeHandles(); err != nil {
		t.Fatal(err)
	}
	session.finishFile(record.LocatorDigest(), transaction)
	return record
}

func outputV3RetirementCutLabel(removed int) string {
	return []string{
		"before-stage-removal",
		"after-stage-removal",
		"after-anchor-removal",
		"after-record-removal",
	}[removed]
}
