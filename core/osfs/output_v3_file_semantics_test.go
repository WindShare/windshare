package osfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3BeginFileSeparatesPreStateCollisionFromReservedRecovery(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelectionPaths(t, []string{"preexisting.bin", "reserved.bin"}, 3)
	var traces []FilesystemOutputTrace
	authority := v3RecoveryAuthority(t, root, nil)
	authority.tracer = FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		traces = append(traces, event)
	})
	opened := v3RecoveryOpen(t, authority, root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })

	reservedFile := v3RecoveryOutputFileAt(t, opened.Session, selection, 1)
	reserved := outputV3SemanticInstallReservedRecord(t, opened.Session, reservedFile)
	for _, selected := range selection.Files() {
		if err := os.WriteFile(filepath.Join(root, selected.Path), []byte("usr"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for index := range selection.Files() {
		file := v3RecoveryOutputFileAt(t, opened.Session, selection, index)
		start, err := opened.Session.BeginFile(context.Background(), file)
		settlement, settled := start.ImmediateSettlement()
		if err != nil || !settled || settlement.Kind() != transfer.FileCollision {
			t.Fatalf("collision %d = (kind=%v, settled=%t, err=%v)", index, settlement.Kind(), settled, err)
		}
		outputV3SemanticRequireRecordAbsent(t, root, selection, opened.Session.SessionID(), file.Path)
	}

	foundReservedRecovery := false
	for _, event := range traces {
		if event.Operation == TraceFileRecoveryDecision &&
			event.LocatorDigest == outputLocatorDigestFromState(reserved.Bound().Record().LocatorDigest()) &&
			event.PreviousPhase == FilesystemOutputFileReserved &&
			event.RecoveryAction == FilesystemOutputRecoveryInstallRetiring {
			foundReservedRecovery = true
			if event.SessionID.IsZero() || event.OutputObjectID.IsZero() || event.ResumeIntent.IsZero() {
				t.Fatalf("reserved collision trace omitted stable authority: %+v", event)
			}
		}
	}
	if !foundReservedRecovery {
		t.Fatal("reserved collision did not trace its durable retirement decision")
	}
}

func TestOutputV3BeginFileQuarantinesOnlyMatchingUnsafeFileNamespaces(t *testing.T) {
	paths := []string{"corrupt.bin", "wrong-type.bin", "orphan-update.bin", "malformed-update.bin", "clean.bin"}
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelectionPaths(t, paths, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })

	corruptName := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(paths[0]))
	outputV3SemanticCreatePrivateFile(t, opened.Session.filesDir, corruptName, nil, 0)

	wrongTypeName := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(paths[1]))
	wrongTypeShard := outputV3SemanticOpenShard(t, opened.Session.filesDir, wrongTypeName.Shard(), true)
	wrongType, err := wrongTypeShard.CreateDirectory(wrongTypeName.Name(), true)
	if err != nil {
		_ = wrongTypeShard.Close()
		t.Fatal(err)
	}
	if err := errors.Join(wrongType.Sync(), wrongType.Close(), wrongTypeShard.Sync(), wrongTypeShard.Close()); err != nil {
		t.Fatal(err)
	}

	nonce, err := resumestate.UpdateNonceFromBytes(bytes.Repeat([]byte{0x71}, resumestate.UpdateNonceBytes))
	if err != nil {
		t.Fatal(err)
	}
	orphanUpdate := resumestate.UpdateTemporaryName(resumestate.DigestCanonicalLocator(paths[2]), nonce)
	outputV3SemanticCreatePrivateFile(t, opened.Session.filesDir, orphanUpdate, nil, 0)
	malformedUpdate := resumestate.UpdateTemporaryName(resumestate.DigestCanonicalLocator(paths[3]), nonce)
	outputV3SemanticCreatePrivateNamedFile(
		t, opened.Session.filesDir, malformedUpdate.Shard(), strings.ToUpper(malformedUpdate.Name()), nil, 0,
	)

	indexByPath := make(map[string]int, len(selection.Files()))
	for index, selected := range selection.Files() {
		indexByPath[selected.Path] = index
	}
	for _, path := range paths[:len(paths)-1] {
		file := v3RecoveryOutputFileAt(t, opened.Session, selection, indexByPath[path])
		start, beginErr := opened.Session.BeginFile(context.Background(), file)
		settlement, settled := start.ImmediateSettlement()
		if beginErr != nil || !settled || settlement.Kind() != transfer.FileQuarantined {
			t.Fatalf("unsafe namespace %q = (kind=%v, settled=%t, err=%v)", path, settlement.Kind(), settled, beginErr)
		}
		_, reason, valid := settlement.Quarantine()
		if !valid || reason != transfer.QuarantineStateCorrupt {
			t.Fatalf("unsafe namespace %q reason = %v, valid=%t", path, reason, valid)
		}
	}

	cleanFile := v3RecoveryOutputFileAt(t, opened.Session, selection, indexByPath[paths[len(paths)-1]])
	clean := v3RecoveryBeginTransaction(t, opened.Session, cleanFile)
	if _, err := clean.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip); err != nil {
		t.Fatalf("sibling file was blocked by another locator's corruption: %v", err)
	}
}

func TestOutputV3BeginFileRejectsInvalidAndDuplicateOwnershipClaims(t *testing.T) {
	var nilSession *filesystemOutputSession
	if _, err := nilSession.BeginFile(context.Background(), transfer.OutputFile{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil session begin error = %v", err)
	}

	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := opened.Session.BeginFile(canceled, file); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled begin error = %v", err)
	}
	invalid := file
	invalid.ExpectedSize++
	_, err := opened.Session.BeginFile(context.Background(), invalid)
	outputV3SemanticRequireFault(t, err, transfer.OutputFaultFile, transfer.OutputFaultContract)

	transaction := v3RecoveryBeginTransaction(t, opened.Session, file)
	_, err = opened.Session.BeginFile(context.Background(), file)
	if !errors.Is(err, errOutputFileActive) {
		t.Fatalf("duplicate begin error = %v", err)
	}
	outputV3SemanticRequireFault(t, err, transfer.OutputFaultFile, transfer.OutputFaultOwnership)
	if _, err := transaction.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip); err != nil {
		t.Fatal(err)
	}
}

func TestOutputV3RecoveryScopesAmbiguousDataBearingCutsByPhase(t *testing.T) {
	t.Run("witnessed-matching-final", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		payload := []byte("owned")
		selection := v3RecoverySelection(t, true, uint64(len(payload)))
		opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
		t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
		file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
		transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)
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
		record := transaction.resumable.Bound().Record()
		stage := resumestate.StageName(record.OutputObject())
		stagePath := filepath.Join(
			v3RecoverySessionPath(root, selection, opened.Session.SessionID()),
			resumestate.StagesDirectoryName, stage.Shard(), stage.Name(),
		)
		if err := os.Link(stagePath, filepath.Join(root, v3RecoveryFilePath)); err != nil {
			t.Fatal(err)
		}
		outputV3SemanticDetachTransaction(t, opened.Session, transaction)

		start, err := opened.Session.BeginFile(context.Background(), file)
		settlement, settled := start.ImmediateSettlement()
		if err != nil || !settled || settlement.Kind() != transfer.FileQuarantined {
			t.Fatalf("ambiguous matching final = (kind=%v, settled=%t, err=%v)", settlement.Kind(), settled, err)
		}
		_, reason, valid := settlement.Quarantine()
		if !valid || reason != transfer.QuarantinePublicationAmbiguous {
			t.Fatalf("matching final quarantine = (reason=%v, valid=%t)", reason, valid)
		}
		actual, readErr := os.ReadFile(filepath.Join(root, v3RecoveryFilePath))
		if readErr != nil || !bytes.Equal(actual, payload) {
			t.Fatalf("ambiguous final was modified: %q, %v", actual, readErr)
		}
	})

	t.Run("retiring-stage-without-anchor", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		selection := v3RecoverySelection(t, true, 1)
		sessionIDs := &v3RecoverySessionIDs{}
		opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
		file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
		record := v3RecoveryPrepareRetiringCut(t, opened.Session, file)
		sessionID := opened.Session.SessionID()
		v3RecoveryCloseSession(t, opened.Session)

		anchor := resumestate.AnchorName(record.OutputObject())
		anchorPath := filepath.Join(
			v3RecoverySessionPath(root, selection, sessionID),
			resumestate.AnchorsDirectoryName, anchor.Shard(), anchor.Name(),
		)
		if err := os.Remove(anchorPath); err != nil {
			t.Fatal(err)
		}
		reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
		t.Cleanup(func() { v3RecoveryCloseSession(t, reopened.Session) })
		start, err := reopened.Session.BeginFile(
			context.Background(), v3RecoveryOutputFile(t, reopened.Session, selection, 1),
		)
		if _, settled := start.ImmediateSettlement(); settled || !outputV3FailureRequiresJobPause(err) ||
			!errors.Is(err, errOutputV3InternalCleanupNeedsAttention) {
			t.Fatalf("asymmetric retirement = (settled=%t, err=%v), want preservation pause", settled, err)
		}
		stage := resumestate.StageName(record.OutputObject())
		stagePath := filepath.Join(
			v3RecoverySessionPath(root, selection, sessionID),
			resumestate.StagesDirectoryName, stage.Shard(), stage.Name(),
		)
		if _, err := os.Stat(stagePath); err != nil {
			t.Fatalf("attention hold removed the remaining data-bearing stage: %v", err)
		}
		persisted := outputV3PersistedFileRecord(t, reopened.Session, record.CanonicalLocator())
		if persisted.Phase() != resumestate.FileRetiring || persisted.QuarantineReason() != 0 {
			t.Fatalf("asymmetric retirement record = (phase=%v, quarantine=%v)",
				persisted.Phase(), persisted.QuarantineReason())
		}
	})
}

func TestOutputV3PublishedCleanupAmbiguityPausesAndPreservesWitnesses(t *testing.T) {
	root := v3RecoveryRoot(t)
	payload := []byte("published")
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	var traces []FilesystemOutputTrace
	authority := v3RecoveryAuthority(t, root, nil)
	authority.tracer = FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		traces = append(traces, event)
	})
	opened := v3RecoveryOpen(t, authority, root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file)
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("initial publication = (kind=%v, err=%v)", settlement.Kind(), err)
	}
	record := outputV3PersistedFileRecord(t, opened.Session, file.Path)
	stage := resumestate.StageName(record.OutputObject())
	stagePath := filepath.Join(
		v3RecoverySessionPath(root, selection, opened.Session.SessionID()),
		resumestate.StagesDirectoryName, stage.Shard(), stage.Name(),
	)
	foreignStage := []byte("foreign-stage")
	if err := os.WriteFile(stagePath, foreignStage, 0o600); err != nil {
		t.Fatal(err)
	}

	start, err := opened.Session.BeginFile(context.Background(), file)
	if _, immediate := start.ImmediateSettlement(); immediate || !outputV3FailureRequiresJobPause(err) ||
		!errors.Is(err, errOutputV3InternalCleanupNeedsAttention) {
		t.Fatalf("published cleanup ambiguity = (immediate=%t, err=%v), want preservation pause", immediate, err)
	}
	outputV3SemanticRequireFault(t, err, transfer.OutputFaultFile, transfer.OutputFaultOwnership)
	persisted := outputV3PersistedFileRecord(t, opened.Session, file.Path)
	if persisted.Phase() != resumestate.FilePublished || persisted.QuarantineReason() != 0 {
		t.Fatalf("published cleanup record = (phase=%v, quarantine=%v)",
			persisted.Phase(), persisted.QuarantineReason())
	}
	if actual, readErr := os.ReadFile(stagePath); readErr != nil || !bytes.Equal(actual, foreignStage) {
		t.Fatalf("foreign stage changed = %q, %v", actual, readErr)
	}
	finalPath := filepath.Join(root, filepath.FromSlash(file.Path))
	if actual, readErr := os.ReadFile(finalPath); readErr != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("published final changed = %q, %v", actual, readErr)
	}
	anchor := resumestate.AnchorName(record.OutputObject())
	anchorPath := filepath.Join(
		v3RecoverySessionPath(root, selection, opened.Session.SessionID()),
		resumestate.AnchorsDirectoryName, anchor.Shard(), anchor.Name(),
	)
	if _, statErr := os.Stat(anchorPath); statErr != nil {
		t.Fatalf("published anchor was removed: %v", statErr)
	}
	for _, event := range traces {
		if event.Operation == TraceFileRecoveryDecision &&
			event.RecoveryAction == FilesystemOutputRecoveryHoldPublishedCleanup {
			return
		}
	}
	t.Fatal("published cleanup hold was not traced")
}

func TestOutputV3ActiveRetirementPausesWhenAStageLosesItsAnchor(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)
	if err := transaction.anchorDir.RemoveFile(transaction.anchorName, transaction.anchor); err != nil {
		t.Fatal(err)
	}
	if err := transaction.anchorDir.Sync(); err != nil {
		t.Fatal(err)
	}

	settlement, err := transaction.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip)
	if settlement.Kind() != 0 || !outputV3FailureRequiresJobPause(err) ||
		!errors.Is(err, errOutputV3InternalCleanupNeedsAttention) {
		t.Fatalf("retirement namespace race = (kind=%v, err=%v), want preservation pause", settlement.Kind(), err)
	}
	record := transaction.resumable.Bound().Record()
	stage := resumestate.StageName(record.OutputObject())
	stagePath := filepath.Join(
		v3RecoverySessionPath(root, selection, opened.Session.SessionID()),
		resumestate.StagesDirectoryName, stage.Shard(), stage.Name(),
	)
	if _, statErr := os.Stat(stagePath); statErr != nil {
		t.Fatalf("attention hold removed the remaining stage: %v", statErr)
	}
	persisted := outputV3PersistedFileRecord(t, opened.Session, record.CanonicalLocator())
	if persisted.Phase() != resumestate.FileRetiring || persisted.QuarantineReason() != 0 {
		t.Fatalf("active retirement record = (phase=%v, quarantine=%v)",
			persisted.Phase(), persisted.QuarantineReason())
	}
}

func TestOutputV3TerminalCloseFailureIsCleanupStateIO(t *testing.T) {
	cleanupCause := errors.New("terminal close cleanup failed")
	primaryCause := errors.New("primary operation failed")
	for _, test := range []struct {
		name      string
		primary   error
		wantScope transfer.OutputFaultScope
		wantCode  transfer.OutputFaultCode
	}{
		{name: "cleanup only", wantScope: transfer.OutputFaultFile, wantCode: transfer.OutputFaultStateIO},
		{
			name: "primary remains first",
			primary: outputAncestrySessionFault(
				"preserve primary result", primaryCause, false,
			),
			wantScope: transfer.OutputFaultSession,
			wantCode:  transfer.OutputFaultNamespaceUnsafe,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, transaction := outputV3PublicationReadyTransaction(t)
			if err := transaction.claimTerminalSettlement(true); err != nil {
				t.Fatal(err)
			}
			transaction.mu.Lock()
			transaction.data = &outputV3PublicationFile{
				outputV3File: transaction.data,
				faults: outputV3PublicationFileFaults{
					closeErr: errors.Join(errOutputV3Unsafe, cleanupCause),
				},
			}
			transaction.mu.Unlock()

			resultErr := test.primary
			transaction.finishTerminalResult(&resultErr, "close terminal output")
			if !errors.Is(resultErr, cleanupCause) || !errors.Is(resultErr, errOutputV3Unsafe) ||
				!outputV3FailureRequiresJobPause(resultErr) {
				t.Fatalf("terminal cleanup result = %v", resultErr)
			}
			if test.primary != nil && !errors.Is(resultErr, primaryCause) {
				t.Fatalf("terminal cleanup lost primary cause: %v", resultErr)
			}
			var firstFault *transfer.OutputFault
			if !errors.As(resultErr, &firstFault) || firstFault.Scope() != test.wantScope ||
				firstFault.Code() != test.wantCode {
				t.Fatalf("first terminal fault = %+v, want %v/%v", firstFault, test.wantScope, test.wantCode)
			}
			var cleanupSession *transfer.OutputSessionError
			if !errors.As(resultErr, &cleanupSession) || !cleanupSession.RequiresJobPause() {
				t.Fatalf("terminal cleanup pause wrapper = %+v", cleanupSession)
			}
			var cleanupFault *transfer.OutputFault
			if !errors.As(cleanupSession.Unwrap(), &cleanupFault) ||
				cleanupFault.Scope() != transfer.OutputFaultFile ||
				cleanupFault.Code() != transfer.OutputFaultStateIO {
				t.Fatalf("terminal cleanup fault = %+v, want File/StateIO", cleanupFault)
			}
		})
	}
}

func TestOutputV3TransactionStartRejectsPostObservationWitnessRaces(t *testing.T) {
	for _, test := range []struct {
		name   string
		fault  string
		reason resumestate.QuarantineReason
	}{
		{name: "stage-shard-missing", fault: "stage-shard", reason: resumestate.QuarantineStageUnsafe},
		{name: "anchor-shard-missing", fault: "anchor-shard", reason: resumestate.QuarantineAnchorUnsafe},
		{name: "stage-open-failure", fault: "stage-open", reason: resumestate.QuarantineStageUnsafe},
		{name: "anchor-open-failure", fault: "anchor-open", reason: resumestate.QuarantineAnchorUnsafe},
		{name: "stage-anchor-identity-mismatch", fault: "identity", reason: resumestate.QuarantineStageUnsafe},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, true, 1)
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
			initial := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)
			resumable := initial.resumable
			record := resumable.Bound().Record()
			outputV3SemanticDetachTransaction(t, opened.Session, initial)
			recordName := resumestate.FileRecordName(record.LocatorDigest())
			recordDir := outputV3SemanticOpenShard(t, opened.Session.filesDir, recordName.Shard(), false)
			stageName := resumestate.StageName(record.OutputObject())
			anchorName := resumestate.AnchorName(record.OutputObject())
			originalStages := opened.Session.stagesDir
			originalAnchors := opened.Session.anchorsDir
			failure := errors.New("witness changed after observation")
			switch test.fault {
			case "stage-shard":
				opened.Session.stagesDir = &outputV3SemanticFaultShardParent{
					outputV3Directory: originalStages, absentShard: stageName.Shard(),
				}
			case "anchor-shard":
				opened.Session.anchorsDir = &outputV3SemanticFaultShardParent{
					outputV3Directory: originalAnchors, absentShard: anchorName.Shard(),
				}
			case "stage-open":
				opened.Session.stagesDir = &outputV3SemanticFaultShardParent{
					outputV3Directory: originalStages, targetShard: stageName.Shard(), childOpenFileErr: failure,
				}
			case "anchor-open":
				opened.Session.anchorsDir = &outputV3SemanticFaultShardParent{
					outputV3Directory: originalAnchors, targetShard: anchorName.Shard(), childOpenFileErr: failure,
				}
			case "identity":
				opened.Session.stagesDir = &outputV3SemanticFaultShardParent{
					outputV3Directory: originalStages, targetShard: stageName.Shard(), childForceDifferent: true,
				}
			}

			start, err := opened.Session.transactionStart(file.Descriptor, resumable, recordDir, recordName.Name())
			opened.Session.stagesDir, opened.Session.anchorsDir = originalStages, originalAnchors
			if err != nil {
				_ = recordDir.Close()
				v3RecoveryCloseSession(t, opened.Session)
				t.Fatalf("witness race quarantine error = %v", err)
			}
			settlement, immediate := start.ImmediateSettlement()
			if !immediate || settlement.Kind() != transfer.FileQuarantined {
				t.Fatalf("witness race settlement = (kind=%v, immediate=%t)", settlement.Kind(), immediate)
			}
			persisted := readOutputV3PublicationAuthorityRecord(t, root, selection, opened.Session.SessionID(), record)
			if persisted.Phase() != resumestate.FileQuarantined || persisted.QuarantineReason() != test.reason {
				t.Fatalf("witness race record = (phase=%v, reason=%v)", persisted.Phase(), persisted.QuarantineReason())
			}
			if err := recordDir.Close(); err != nil {
				t.Fatal(err)
			}
			v3RecoveryCloseSession(t, opened.Session)

			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			t.Cleanup(func() { v3RecoveryCloseSession(t, reopened.Session) })
			retry, retryErr := reopened.Session.BeginFile(context.Background(), v3RecoveryOutputFile(t, reopened.Session, selection, 1))
			retrySettlement, retryImmediate := retry.ImmediateSettlement()
			if retryErr != nil || !retryImmediate || retrySettlement.Kind() != transfer.FileQuarantined {
				t.Fatalf("witness race restart = (kind=%v, immediate=%t, err=%v)",
					retrySettlement.Kind(), retryImmediate, retryErr)
			}
		})
	}
}

func TestOutputV3PublishBlockedRetriesWithoutReceivingContent(t *testing.T) {
	root := v3RecoveryRoot(t)
	payload := []byte("owned-output")
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file)
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	foreign := bytes.Repeat([]byte{'x'}, len(payload))
	finalPath := filepath.Join(root, v3RecoveryFilePath)
	if err := os.WriteFile(finalPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	blocked, err := transaction.Commit(context.Background())
	if err != nil || blocked.Kind() != transfer.FilePublishBlocked {
		t.Fatalf("initial collision = (kind=%v, err=%v)", blocked.Kind(), err)
	}
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}

	start, err := opened.Session.BeginFile(context.Background(), file)
	settlement, settled := start.ImmediateSettlement()
	if err != nil || !settled || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("collision retry = (kind=%v, settled=%t, err=%v)", settlement.Kind(), settled, err)
	}
	actual, readErr := os.ReadFile(finalPath)
	if readErr != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("retried publication = %q, %v, want %q", actual, readErr, payload)
	}
}

func TestOutputV3CheckpointFailuresPreserveLastVerifiedAuthority(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 3)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, 3)
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)

	originalData := transaction.data
	transaction.data = &outputV3SemanticFaultFile{outputV3File: originalData, shortWrite: true}
	err := transaction.WriteRange(context.Background(), 0, []byte("a"))
	outputV3SemanticRequireFault(t, err, transfer.OutputFaultFile, transfer.OutputFaultStateIO)
	if !transaction.pending.IsEmpty() {
		t.Fatalf("short write acquired pending range authority: %v", transaction.pending.Ranges())
	}
	transaction.data = originalData
	if err := transaction.WriteRange(context.Background(), 0, []byte("a")); err != nil {
		t.Fatal(err)
	}

	syncFailure := errors.New("checkpoint sync failed")
	transaction.data = &outputV3SemanticFaultFile{outputV3File: originalData, syncErr: syncFailure}
	_, err = transaction.Checkpoint(context.Background())
	if !errors.Is(err, syncFailure) {
		t.Fatalf("checkpoint sync error = %v", err)
	}
	outputV3SemanticRequireFault(t, err, transfer.OutputFaultFile, transfer.OutputFaultStateIO)
	outputV3SemanticRequireCheckpointState(t, transaction, 0, 1)
	transaction.data = originalData
	checkpoint, err := transaction.Checkpoint(context.Background())
	if err != nil || checkpoint.CheckpointGeneration() != 1 {
		t.Fatalf("checkpoint after sync recovery = (generation=%d, err=%v)", checkpoint.CheckpointGeneration(), err)
	}

	if err := transaction.WriteRange(context.Background(), 1, []byte("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := transaction.WriteRange(context.Background(), 2, []byte("c")); err != nil {
		t.Fatal(err)
	}
	originalRecordDir := transaction.recordDir
	installFailure := errors.New("checkpoint record install failed")
	transaction.recordDir = &outputV3SemanticFaultDirectory{
		outputV3Directory: originalRecordDir,
		createFileErr:     installFailure,
	}
	_, err = transaction.Checkpoint(context.Background())
	if !errors.Is(err, installFailure) {
		t.Fatalf("checkpoint state-install error = %v", err)
	}
	outputV3SemanticRequireFault(t, err, transfer.OutputFaultFile, transfer.OutputFaultStateIO)
	outputV3SemanticRequireCheckpointState(t, transaction, 2, 1)
	transaction.recordDir = originalRecordDir
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}

	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("commit after checkpoint recovery = (kind=%v, err=%v)", settlement.Kind(), err)
	}
	actual, readErr := os.ReadFile(filepath.Join(root, v3RecoveryFilePath))
	if readErr != nil || !bytes.Equal(actual, []byte("abc")) {
		t.Fatalf("published bytes = %q, %v", actual, readErr)
	}
	info, statErr := os.Stat(filepath.Join(root, v3RecoveryFilePath))
	if statErr != nil || info.ModTime().Unix() != file.Descriptor.ModifiedTime().Seconds() {
		t.Fatalf("published modified time = %v, %v", info.ModTime(), statErr)
	}
}

func TestOutputV3CheckpointWitnessComparisonSeparatesDenialFromContradiction(t *testing.T) {
	for _, test := range []struct {
		name       string
		cause      error
		different  bool
		quarantine bool
	}{
		{name: "raw permission pauses", cause: fs.ErrPermission},
		{name: "explicit unsafe quarantines", cause: errors.Join(errOutputV3Unsafe, errors.New("identity unavailable")), quarantine: true},
		{name: "identity mismatch quarantines", different: true, quarantine: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, true, 1)
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
			transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)
			if err := transaction.WriteRange(context.Background(), 0, []byte{1}); err != nil {
				t.Fatal(err)
			}
			record := transaction.resumable.Bound().Record()
			originalData := transaction.data
			transaction.data = &outputV3SemanticFaultFile{
				outputV3File: originalData, sameErr: test.cause, forceDifferent: test.different,
			}

			_, checkpointErr := transaction.Checkpoint(context.Background())
			if !outputV3FailureRequiresJobPause(checkpointErr) {
				t.Fatalf("checkpoint witness error does not require PauseJob: %v", checkpointErr)
			}
			if test.cause != nil && !errors.Is(checkpointErr, test.cause) {
				t.Fatalf("checkpoint witness error = %v, want %v", checkpointErr, test.cause)
			}
			persisted := readOutputV3PublicationAuthorityRecord(t, root, selection, opened.Session.SessionID(), record)
			if !test.quarantine {
				if persisted.Phase() != resumestate.FileWitnessed || persisted.StateGeneration() != record.StateGeneration() {
					t.Fatalf("permission checkpoint record = (phase=%v, generation=%d)",
						persisted.Phase(), persisted.StateGeneration())
				}
				v3RecoveryCloseSession(t, opened.Session)
				return
			}
			if persisted.Phase() != resumestate.FileQuarantined ||
				persisted.QuarantineReason() != resumestate.QuarantineStageUnsafe {
				t.Fatalf("checkpoint witness quarantine = (phase=%v, reason=%v)",
					persisted.Phase(), persisted.QuarantineReason())
			}
			v3RecoveryCloseSession(t, opened.Session)
			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			t.Cleanup(func() { v3RecoveryCloseSession(t, reopened.Session) })
			retry, retryErr := reopened.Session.BeginFile(
				context.Background(), v3RecoveryOutputFile(t, reopened.Session, selection, 1),
			)
			retrySettlement, immediate := retry.ImmediateSettlement()
			if retryErr != nil || !immediate || retrySettlement.Kind() != transfer.FileQuarantined {
				t.Fatalf("checkpoint witness restart = (kind=%v/%t, err=%v)",
					retrySettlement.Kind(), immediate, retryErr)
			}
		})
	}
}

func TestOutputV3WitnessCreationCleanupCannotChooseRecoveryTaxonomy(t *testing.T) {
	rawFailure := errors.New("witness creation operation denied")
	unsafeCause := errors.New("witness creation identity contradiction")
	cleanupCause := errors.New("witness creation close diagnostic")
	unsafeOperation := errors.Join(errOutputV3Unsafe, unsafeCause)
	unsafeCleanup := errors.Join(errOutputV3Unsafe, cleanupCause)

	for _, test := range []struct {
		name       string
		primary    error
		quarantine bool
	}{
		{name: "fully verified cut"},
		{name: "raw primary", primary: rawFailure},
		{name: "unsafe primary", primary: unsafeOperation, quarantine: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, true, 1)
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			active := opened.Session
			t.Cleanup(func() {
				if active != nil {
					v3RecoveryCloseSession(t, active)
				}
			})
			file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
			anchorSyncCalls := 0
			anchorFaults := &outputV3WitnessCreationDirectory{
				outputV3Directory: opened.Session.anchorsDir,
				syncErr:           test.primary,
				syncErrAt:         3,
				syncCalls:         &anchorSyncCalls,
				linkedCloseErr:    unsafeCleanup,
			}
			originalAnchors := opened.Session.anchorsDir
			opened.Session.anchorsDir = anchorFaults

			start, beginErr := opened.Session.BeginFile(context.Background(), file)
			opened.Session.anchorsDir = originalAnchors
			if !errors.Is(beginErr, cleanupCause) || !outputV3FailureRequiresJobPause(beginErr) {
				t.Fatalf("witness creation cleanup = %v", beginErr)
			}
			if test.primary == rawFailure && !errors.Is(beginErr, rawFailure) {
				t.Fatalf("raw witness creation cause omitted: %v", beginErr)
			}
			if _, _, transaction := start.Transaction(); transaction {
				t.Fatal("witness creation cleanup returned a content transaction")
			}
			if _, immediate := start.ImmediateSettlement(); immediate {
				t.Fatal("witness creation cleanup returned an immediate settlement")
			}
			persisted := outputV3PersistedFileRecord(t, opened.Session, file.Path)
			if test.quarantine {
				if persisted.Phase() != resumestate.FileQuarantined ||
					persisted.QuarantineReason() != resumestate.QuarantinePartialObjectCreation {
					t.Fatalf("unsafe witness creation record = (phase=%v, reason=%v)",
						persisted.Phase(), persisted.QuarantineReason())
				}
			} else if persisted.Phase() != resumestate.FileReserved {
				t.Fatalf("witness cleanup changed record phase to %v", persisted.Phase())
			}

			v3RecoveryCloseSession(t, active)
			active = nil
			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			t.Cleanup(func() { v3RecoveryCloseSession(t, reopened.Session) })
			retry, retryErr := reopened.Session.BeginFile(
				context.Background(), v3RecoveryOutputFile(t, reopened.Session, selection, 1),
			)
			if test.quarantine {
				settlement, immediate := retry.ImmediateSettlement()
				if retryErr != nil || !immediate || settlement.Kind() != transfer.FileQuarantined {
					t.Fatalf("unsafe witness creation restart = (kind=%v/%t, err=%v)",
						settlement.Kind(), immediate, retryErr)
				}
				return
			}
			transaction, _, resumed := retry.Transaction()
			if retryErr != nil || !resumed {
				t.Fatalf("deterministic witness restart = (resumed=%t, err=%v)", resumed, retryErr)
			}
			settlement, retireErr := transaction.Retire(
				context.Background(), transfer.FileRetireExplicitPolicySkip,
			)
			if retireErr != nil || settlement.Kind() != transfer.FileRetired {
				t.Fatalf("retire recovered witness = (kind=%v, err=%v)", settlement.Kind(), retireErr)
			}
		})
	}
}

func TestOutputV3MetadataFailureResumesCompleteCheckpoint(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*outputV3SemanticFaultFile, error)
		wantSync  int
	}{
		{
			name: "set-modified-time",
			configure: func(file *outputV3SemanticFaultFile, failure error) {
				file.setModifiedErr = failure
			},
		},
		{
			name: "metadata-sync",
			configure: func(file *outputV3SemanticFaultFile, failure error) {
				file.syncErr = failure
			},
			wantSync: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			payload := []byte("data")
			selection := v3RecoverySelection(t, true, uint64(len(payload)))
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
			t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
			file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
			transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)
			if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
				t.Fatal(err)
			}
			// Pre-checkpointing isolates the second Sync as the metadata durability cut.
			if _, err := transaction.Checkpoint(context.Background()); err != nil {
				t.Fatal(err)
			}
			metadataFailure := errors.New("metadata install failed")
			faultData := &outputV3SemanticFaultFile{outputV3File: transaction.data}
			test.configure(faultData, metadataFailure)
			transaction.data = faultData
			_, err := transaction.Commit(context.Background())
			if faultData.setModifiedCalls != 1 || faultData.syncCalls != test.wantSync {
				t.Fatalf(
					"metadata calls = set:%d sync:%d, want set:1 sync:%d",
					faultData.setModifiedCalls, faultData.syncCalls, test.wantSync,
				)
			}
			if !errors.Is(err, metadataFailure) {
				t.Fatalf("metadata failure = %v", err)
			}
			outputV3SemanticRequireFault(t, err, transfer.OutputFaultFile, transfer.OutputFaultStateIO)
			if transaction.resumable.Bound().Record().Phase() != resumestate.FileWitnessed {
				t.Fatalf("metadata failure advanced file phase to %v", transaction.resumable.Bound().Record().Phase())
			}
			if _, err := os.Stat(filepath.Join(root, v3RecoveryFilePath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("metadata failure published a final path: %v", err)
			}

			start, err := opened.Session.BeginFile(context.Background(), file)
			resumed, durable, ok := start.Transaction()
			if err != nil || !ok || !transfer.RangesCoverFile(uint64(len(payload)), durable.Ranges()) {
				t.Fatalf(
					"resume after metadata failure = (transaction=%t, ranges=%v, err=%v)",
					ok, durable.Ranges().Ranges(), err,
				)
			}
			paused, err := resumed.Pause(context.Background(), transfer.FilePauseOutputFailure)
			pausedCheckpoint, verified := paused.VerifiedCheckpoint()
			if err != nil || paused.Kind() != transfer.FilePaused || !verified ||
				!transfer.RangesCoverFile(uint64(len(payload)), pausedCheckpoint.Ranges()) {
				t.Fatalf(
					"pause after metadata failure = (kind=%v, verified=%t, ranges=%v, err=%v)",
					paused.Kind(), verified, pausedCheckpoint.Ranges().Ranges(), err,
				)
			}
			start, err = opened.Session.BeginFile(context.Background(), file)
			resumed, durable, ok = start.Transaction()
			if err != nil || !ok || !transfer.RangesCoverFile(uint64(len(payload)), durable.Ranges()) {
				t.Fatalf(
					"resume after safe pause = (transaction=%t, ranges=%v, err=%v)",
					ok, durable.Ranges().Ranges(), err,
				)
			}
			settlement, err := resumed.Commit(context.Background())
			if err != nil || settlement.Kind() != transfer.FilePublished {
				t.Fatalf("publish recovered checkpoint = (kind=%v, err=%v)", settlement.Kind(), err)
			}
			actual, readErr := os.ReadFile(filepath.Join(root, v3RecoveryFilePath))
			if readErr != nil || !bytes.Equal(actual, payload) {
				t.Fatalf("recovered publication = %q, %v", actual, readErr)
			}
		})
	}
}

func TestOutputV3TransactionRejectsBoundsAndInvalidSettlementsWithoutMutation(t *testing.T) {
	var nilTransaction *filesystemFileTransaction
	if nilTransaction.Binding().BackendID() != "" {
		t.Fatal("nil transaction exposed a binding")
	}
	if err := nilTransaction.WriteRange(context.Background(), 0, []byte("x")); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil write error = %v", err)
	}
	if _, err := nilTransaction.Checkpoint(context.Background()); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil checkpoint error = %v", err)
	}
	if _, err := nilTransaction.Commit(context.Background()); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil commit error = %v", err)
	}
	if err := nilTransaction.closeHandles(); err != nil {
		t.Fatalf("nil cleanup error = %v", err)
	}

	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 2)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, 2)
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)
	if err := transaction.WriteRange(context.Background(), math.MaxUint64, nil); err != nil {
		t.Fatalf("empty range should not acquire or require file authority: %v", err)
	}
	for _, write := range []struct {
		offset uint64
		data   []byte
	}{
		{offset: 2, data: []byte("x")},
		{offset: math.MaxInt64 + 1, data: []byte("x")},
		{offset: 1, data: []byte("xy")},
	} {
		if err := transaction.WriteRange(context.Background(), write.offset, write.data); !errors.Is(err, ErrOutOfRange) {
			t.Fatalf("out-of-range write (%d,%d) error = %v", write.offset, len(write.data), err)
		}
	}
	checkpoint, err := transaction.Checkpoint(context.Background())
	if err != nil || checkpoint.CheckpointGeneration() != 0 || !checkpoint.Ranges().IsEmpty() {
		t.Fatalf("empty checkpoint = (generation=%d, ranges=%v, err=%v)", checkpoint.CheckpointGeneration(), checkpoint.Ranges().Ranges(), err)
	}
	if _, err := transaction.Pause(context.Background(), 0); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("invalid pause error = %v", err)
	}
	if _, err := transaction.Retire(context.Background(), 0); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("invalid retire error = %v", err)
	}
	if _, err := transaction.Commit(context.Background()); !errors.Is(err, transfer.ErrIncompleteOutputFile) {
		t.Fatalf("incomplete commit error = %v", err)
	}

	start, err := opened.Session.BeginFile(context.Background(), file)
	resumed, durable, ok := start.Transaction()
	if err != nil || !ok || !durable.Ranges().IsEmpty() {
		t.Fatalf("resume after incomplete commit = (transaction=%t, ranges=%v, err=%v)", ok, durable.Ranges().Ranges(), err)
	}
	if _, err := resumed.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip); err != nil {
		t.Fatal(err)
	}
}

func TestOutputV3ObjectAllocationSkipsZeroClaimsAndOccupiedNames(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })

	zero := resumestate.OutputObjectID{}
	claimed := outputV3SemanticObjectID(t, 0x11)
	anchorOccupied := outputV3SemanticObjectID(t, 0x22)
	stageOccupied := outputV3SemanticObjectID(t, 0x33)
	unique := outputV3SemanticObjectID(t, 0x44)
	outputV3SemanticCreatePrivateFile(t, opened.Session.anchorsDir, resumestate.AnchorName(anchorOccupied), nil, 0)
	outputV3SemanticCreatePrivateFile(t, opened.Session.stagesDir, resumestate.StageName(stageOccupied), nil, 0)
	otherDigest := resumestate.DigestCanonicalLocator("other.bin")
	targetDigest := resumestate.DigestCanonicalLocator("target.bin")
	opened.Session.mu.Lock()
	opened.Session.objectClaims[claimed] = otherDigest
	opened.Session.mu.Unlock()
	opened.Session.owner.objectIDs = &outputV3SemanticObjectIDs{values: []resumestate.OutputObjectID{
		zero, claimed, anchorOccupied, stageOccupied, unique,
	}}

	allocated, err := opened.Session.allocateOutputObjectID(targetDigest)
	if err != nil || allocated != unique {
		t.Fatalf("allocated object = %v, %v, want %v", allocated, err, unique)
	}
	opened.Session.mu.Lock()
	_, anchorClaimed := opened.Session.objectClaims[anchorOccupied]
	_, stageClaimed := opened.Session.objectClaims[stageOccupied]
	uniqueOwner := opened.Session.objectClaims[unique]
	opened.Session.mu.Unlock()
	if anchorClaimed || stageClaimed || uniqueOwner != targetDigest {
		t.Fatalf("allocation claims = anchor:%t stage:%t unique:%v", anchorClaimed, stageClaimed, uniqueOwner)
	}
	opened.Session.releaseOutputObjectClaim(unique, otherDigest)
	opened.Session.mu.Lock()
	stillClaimed := opened.Session.objectClaims[unique] == targetDigest
	opened.Session.mu.Unlock()
	if !stillClaimed {
		t.Fatal("a different locator released the allocated object claim")
	}
	opened.Session.releaseOutputObjectClaim(unique, targetDigest)
	opened.Session.mu.Lock()
	_, stillClaimed = opened.Session.objectClaims[unique]
	opened.Session.mu.Unlock()
	if stillClaimed {
		t.Fatal("the owning locator could not release its object claim")
	}
}

func TestOutputV3PublicationWitnessCleanupClosesEveryHandleOnce(t *testing.T) {
	stageErr := errors.New("stage close failed")
	anchorErr := errors.New("anchor close failed")
	stage := &outputV3SemanticFaultFile{closeErr: stageErr}
	anchor := &outputV3SemanticFaultFile{closeErr: anchorErr}
	witness := &outputPublicationWitness{stage: stage, anchor: anchor}
	if err := witness.Close(); !errors.Is(err, stageErr) || !errors.Is(err, anchorErr) {
		t.Fatalf("joined witness close error = %v", err)
	}
	if stage.closeCalls != 1 || anchor.closeCalls != 1 {
		t.Fatalf("close calls = stage:%d anchor:%d", stage.closeCalls, anchor.closeCalls)
	}
	if err := witness.Close(); err != nil || stage.closeCalls != 1 || anchor.closeCalls != 1 {
		t.Fatalf("idempotent witness close = %v, calls=%d/%d", err, stage.closeCalls, anchor.closeCalls)
	}
	if err := (*outputPublicationWitness)(nil).Close(); err != nil {
		t.Fatalf("nil witness close = %v", err)
	}
}

type outputV3SemanticFaultFile struct {
	outputV3File
	shortWrite       bool
	syncErr          error
	syncCalls        int
	setModifiedErr   error
	setModifiedCalls int
	forceDifferent   bool
	sameErr          error
	closeErr         error
	closeCalls       int
}

func (file *outputV3SemanticFaultFile) WriteAt(data []byte, offset int64) (int, error) {
	if file.shortWrite {
		return max(0, len(data)-1), nil
	}
	return file.outputV3File.WriteAt(data, offset)
}

func (file *outputV3SemanticFaultFile) Sync() error {
	file.syncCalls++
	if file.syncErr != nil {
		return file.syncErr
	}
	return file.outputV3File.Sync()
}

func (file *outputV3SemanticFaultFile) SetModifiedTime(modified catalog.ModifiedTime) error {
	file.setModifiedCalls++
	if file.setModifiedErr != nil {
		return file.setModifiedErr
	}
	return file.outputV3File.SetModifiedTime(modified)
}

func (file *outputV3SemanticFaultFile) SameFile(other outputV3File) (bool, error) {
	if file.sameErr != nil {
		return false, file.sameErr
	}
	if file.forceDifferent {
		return false, nil
	}
	return file.outputV3File.SameFile(other)
}

func (file *outputV3SemanticFaultFile) Close() error {
	file.closeCalls++
	if file.outputV3File == nil {
		return file.closeErr
	}
	return errors.Join(file.outputV3File.Close(), file.closeErr)
}

type outputV3SemanticFaultDirectory struct {
	outputV3Directory
	createFileErr error
}

func (directory *outputV3SemanticFaultDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputV3File, error) {
	if directory.createFileErr != nil {
		return nil, directory.createFileErr
	}
	return directory.outputV3Directory.CreateFile(name, private, size)
}

type outputV3WitnessCreationDirectory struct {
	outputV3Directory
	syncErr        error
	syncErrAt      int
	syncCalls      *int
	linkedCloseErr error
}

func (directory *outputV3WitnessCreationDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	if err != nil {
		return opened, err
	}
	return directory.wrap(opened), nil
}

func (directory *outputV3WitnessCreationDirectory) CreateDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	created, err := directory.outputV3Directory.CreateDirectory(name, private)
	if err != nil {
		return created, err
	}
	return directory.wrap(created), nil
}

func (directory *outputV3WitnessCreationDirectory) LinkFileNoReplace(
	source outputV3File,
	name string,
) (outputV3File, error) {
	linked, err := directory.outputV3Directory.LinkFileNoReplace(source, name)
	if linked == nil {
		return nil, err
	}
	return &outputV3WitnessCreationFile{
		outputV3File: linked,
		closeErr:     directory.linkedCloseErr,
	}, err
}

func (directory *outputV3WitnessCreationDirectory) Sync() error {
	(*directory.syncCalls)++
	if directory.syncErr != nil && *directory.syncCalls == directory.syncErrAt {
		return directory.syncErr
	}
	return directory.outputV3Directory.Sync()
}

func (directory *outputV3WitnessCreationDirectory) wrap(
	child outputV3Directory,
) *outputV3WitnessCreationDirectory {
	return &outputV3WitnessCreationDirectory{
		outputV3Directory: child,
		syncErr:           directory.syncErr,
		syncErrAt:         directory.syncErrAt,
		syncCalls:         directory.syncCalls,
		linkedCloseErr:    directory.linkedCloseErr,
	}
}

type outputV3WitnessCreationFile struct {
	outputV3File
	closeErr error
}

func (file *outputV3WitnessCreationFile) Close() error {
	return errors.Join(file.outputV3File.Close(), file.closeErr)
}

// outputV3SemanticFaultShardParent models a fixed parent whose child changes
// after recovery observation but before transactionStart reopens the witness.
type outputV3SemanticFaultShardParent struct {
	outputV3Directory
	absentShard         string
	targetShard         string
	childOpenFileErr    error
	childForceDifferent bool
}

func (directory *outputV3SemanticFaultShardParent) ClassifyExactEntry(
	name string,
) (outputV3EntryKind, bool, error) {
	if name == directory.absentShard {
		return outputV3EntryAbsent, true, nil
	}
	return directory.outputV3Directory.ClassifyExactEntry(name)
}

func (directory *outputV3SemanticFaultShardParent) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	if err != nil || name != directory.targetShard {
		return opened, err
	}
	return &outputV3SemanticFaultShard{
		outputV3Directory: opened,
		openFileErr:       directory.childOpenFileErr,
		forceDifferent:    directory.childForceDifferent,
	}, nil
}

type outputV3SemanticFaultShard struct {
	outputV3Directory
	openFileErr    error
	forceDifferent bool
}

func (directory *outputV3SemanticFaultShard) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputV3File, error) {
	if directory.openFileErr != nil {
		return nil, directory.openFileErr
	}
	opened, err := directory.outputV3Directory.OpenFile(name, private, writable)
	if err != nil || !directory.forceDifferent {
		return opened, err
	}
	return &outputV3SemanticFaultFile{outputV3File: opened, forceDifferent: true}, nil
}

type outputV3SemanticObjectIDs struct {
	values []resumestate.OutputObjectID
	next   int
}

func (ids *outputV3SemanticObjectIDs) NewOutputObjectID() (resumestate.OutputObjectID, error) {
	if ids.next >= len(ids.values) {
		return resumestate.OutputObjectID{}, io.EOF
	}
	value := ids.values[ids.next]
	ids.next++
	return value, nil
}

func outputV3SemanticInstallReservedRecord(
	t *testing.T,
	session *filesystemOutputSession,
	file transfer.OutputFile,
) resumestate.ResumableFileAuthority {
	t.Helper()
	digest := resumestate.DigestCanonicalLocator(file.Path)
	objectID, err := session.allocateOutputObjectID(digest)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := resumestate.NewFileRecord(resumestate.FileRecordSpec{
		Session: session.stateSnapshot(), Descriptor: file.Descriptor,
		CanonicalLocator: file.Path, OutputObject: objectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	installFileNamespaceTestRecord(t, session, authority.Bound())
	return authority
}

func outputV3SemanticOpenShard(
	t *testing.T,
	parent outputV3Directory,
	name string,
	create bool,
) outputV3Directory {
	t.Helper()
	shard, present, err := openOutputShard(parent, name, create)
	if err != nil || !present {
		t.Fatalf("open shard %q = (present=%t, err=%v)", name, present, err)
	}
	return shard
}

func outputV3SemanticCreatePrivateFile(
	t *testing.T,
	parent outputV3Directory,
	name resumestate.ShardedName,
	payload []byte,
	size int64,
) {
	t.Helper()
	outputV3SemanticCreatePrivateNamedFile(t, parent, name.Shard(), name.Name(), payload, size)
}

func outputV3SemanticCreatePrivateNamedFile(
	t *testing.T,
	parent outputV3Directory,
	shardName string,
	fileName string,
	payload []byte,
	size int64,
) {
	t.Helper()
	shard := outputV3SemanticOpenShard(t, parent, shardName, true)
	file, err := shard.CreateFile(fileName, true, size)
	if err != nil {
		_ = shard.Close()
		t.Fatal(err)
	}
	if len(payload) != 0 {
		written, writeErr := file.WriteAt(payload, 0)
		if writeErr != nil || written != len(payload) {
			_ = file.Close()
			_ = shard.Close()
			t.Fatalf("write private file = (%d, %v)", written, writeErr)
		}
	}
	if err := errors.Join(file.Sync(), shard.Sync(), file.Close(), shard.Close()); err != nil {
		t.Fatal(err)
	}
}

func outputV3SemanticRequireRecordAbsent(
	t *testing.T,
	root string,
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
	locator string,
) {
	t.Helper()
	name := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(locator))
	path := filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID),
		resumestate.FilesDirectoryName, name.Shard(), name.Name(),
	)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file record %q stat error = %v, want absent", locator, err)
	}
}

func outputV3SemanticDetachTransaction(
	t *testing.T,
	session *filesystemOutputSession,
	transaction *filesystemFileTransaction,
) {
	t.Helper()
	record := transaction.resumable.Bound().Record()
	transaction.lifecycle = filesystemFileTransactionClosed
	if err := transaction.closeHandles(); err != nil {
		t.Fatal(err)
	}
	session.finishFile(record.LocatorDigest(), transaction)
}

func outputV3SemanticRequireCheckpointState(
	t *testing.T,
	transaction *filesystemFileTransaction,
	wantGeneration uint64,
	wantPending int,
) {
	t.Helper()
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	record := transaction.resumable.Bound().Record()
	if record.CheckpointGeneration() != wantGeneration || transaction.pending.Len() != wantPending {
		t.Fatalf(
			"checkpoint authority = generation:%d pending:%v, want generation:%d pending:%d",
			record.CheckpointGeneration(), transaction.pending.Ranges(), wantGeneration, wantPending,
		)
	}
}

func outputV3SemanticRequireFault(
	t *testing.T,
	err error,
	scope transfer.OutputFaultScope,
	code transfer.OutputFaultCode,
) {
	t.Helper()
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != scope || fault.Code() != code {
		t.Fatalf("output fault = %v, want scope=%v code=%v", err, scope, code)
	}
}

func outputV3SemanticObjectID(t *testing.T, value byte) resumestate.OutputObjectID {
	t.Helper()
	id, err := resumestate.OutputObjectIDFromBytes(bytes.Repeat([]byte{value}, resumestate.OutputObjectIDBytes))
	if err != nil {
		t.Fatal(err)
	}
	return id
}
