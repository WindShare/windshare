package outputruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3SettlementEntryFailuresAreTypedAtSource(t *testing.T) {
	var nilTransaction *FileTransaction
	assertOutputV3Fault(t, outputV3CommitError(nilTransaction, context.Background()), transfer.OutputFaultFile, transfer.OutputFaultContract)
	assertOutputV3Fault(
		t, outputV3RetireError(nilTransaction, context.Background(), transfer.FileRetireInvalidatedRevision),
		transfer.OutputFaultFile, transfer.OutputFaultContract,
	)
	assertOutputV3Fault(
		t, outputV3PauseError(nilTransaction, context.Background(), transfer.FilePauseInterrupted),
		transfer.OutputFaultFile, transfer.OutputFaultContract,
	)

	var nilSession *Session
	_, err := nilSession.CompleteJob(context.Background(), transfer.JobSucceeded)
	assertOutputV3Fault(t, err, transfer.OutputFaultSession, transfer.OutputFaultContract)
	_, err = nilSession.PauseJob(context.Background(), transfer.JobPauseInterrupted)
	assertOutputV3Fault(t, err, transfer.OutputFaultSession, transfer.OutputFaultContract)
}

func TestOutputV3ExpiredSettlementContextsRemainTyped(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 8)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, &v3RecoverySessionIDs{}), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, 8)
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*FileTransaction)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	assertOutputV3Fault(t, outputV3CommitError(transaction, expired), transfer.OutputFaultFile, transfer.OutputFaultStateIO)
	assertOutputV3Fault(
		t, outputV3RetireError(transaction, expired, transfer.FileRetireInvalidatedRevision),
		transfer.OutputFaultFile, transfer.OutputFaultStateIO,
	)
	outputV3AbandonTransaction(t, transaction)

	pauseTransaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*FileTransaction)
	settlement, err := pauseTransaction.Pause(expired, transfer.FilePauseInterrupted)
	if settlement.Kind() != transfer.FilePaused {
		t.Fatalf("expired pause settlement = %v", settlement.Kind())
	}
	assertOutputV3Fault(t, err, transfer.OutputFaultFile, transfer.OutputFaultStateIO)
}

func TestOutputV3PausePreservesCheckpointFaultClassification(t *testing.T) {
	t.Run("ownership", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		selection := v3RecoverySelection(t, true, 8)
		opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, &v3RecoverySessionIDs{}), root, selection)
		t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
		transaction := v3RecoveryBeginTransaction(
			t, opened.Session, v3RecoveryOutputFile(t, opened.Session, selection, 8),
		).(*FileTransaction)
		if err := transaction.data.Close(); err != nil {
			t.Fatal(err)
		}
		transaction.data = stagedData{}
		_, err := transaction.Pause(context.Background(), transfer.FilePauseOutputFailure)
		assertOutputV3Fault(t, err, transfer.OutputFaultFile, transfer.OutputFaultOwnership)
	})

	t.Run("contract", func(t *testing.T) {
		exactSize := uint64((resumestate.MaxDurableRangesPerFile + 1) * 2)
		root := v3RecoveryRoot(t)
		selection := v3RecoverySelection(t, true, exactSize)
		opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, &v3RecoverySessionIDs{}), root, selection)
		t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
		transaction := v3RecoveryBeginTransaction(
			t, opened.Session, v3RecoveryOutputFile(t, opened.Session, selection, exactSize),
		).(*FileTransaction)
		ranges := make([]content.Range, resumestate.MaxDurableRangesPerFile+1)
		for index := range ranges {
			offset := uint64(index * 2)
			ranges[index] = content.Range{Offset: offset, End: offset + 1}
		}
		pending, err := content.NewRangeSet(ranges)
		if err != nil {
			t.Fatal(err)
		}
		transaction.pending = pending
		_, err = transaction.Pause(context.Background(), transfer.FilePauseOutputFailure)
		assertOutputV3Fault(t, err, transfer.OutputFaultFile, transfer.OutputFaultContract)
	})
}

func TestOutputV3ExpiredCompleteJobFailureIsTyped(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, &v3RecoverySessionIDs{}), root, selection)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := opened.Session.CompleteJob(expired, transfer.JobSucceeded)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expired CompleteJob error = %v", err)
	}
	assertOutputV3Fault(t, err, transfer.OutputFaultSession, transfer.OutputFaultStateIO)
}

func TestOutputV3StateNamespaceQuarantineReasonsMapToStateCorrupt(t *testing.T) {
	for _, reason := range []resumestate.QuarantineReason{
		resumestate.QuarantineUpdateTemporary,
		resumestate.QuarantineOutputObjectDuplicate,
	} {
		if got := mapQuarantineReason(reason); got != transfer.QuarantineStateCorrupt {
			t.Fatalf("quarantine reason %v maps to %v", reason, got)
		}
	}
	if got := mapQuarantineReason(resumestate.QuarantinePartialObjectCreation); got != transfer.QuarantineRetirementMismatch {
		t.Fatalf("partial-object quarantine maps to %v", got)
	}
}

func TestOutputV3RecoveryFailureClassifierKeepsEvidenceAndDenialDistinct(t *testing.T) {
	denied := errors.New("recovery denied")
	for _, test := range []struct {
		name     string
		cause    error
		boundary outputV3RecoveryBoundary
		want     outputV3RecoveryFailureDisposition
	}{
		{name: "pre-evidence denial", cause: denied, boundary: outputV3BeforeEntryEvidence, want: outputV3RecoveryPauseRequired},
		{name: "authorized mutation denial", cause: denied, boundary: outputV3AuthorizedMutation, want: outputV3RecoveryPauseRequired},
		{name: "unclassified existing entry", cause: denied, boundary: outputV3ExistingEntryUnclassified, want: outputV3RecoveryAmbiguous},
		{name: "positive entry marker", cause: errors.Join(outputnamespace.ErrPositiveEntryEvidence, denied), boundary: outputV3BeforeEntryEvidence, want: outputV3RecoveryAmbiguous},
		{name: "identity contradiction", cause: errors.Join(outputcap.ErrUnsafeNamespace, denied), boundary: outputV3AuthorizedMutation, want: outputV3RecoveryAmbiguous},
		{name: "fixed-name collision", cause: outputcap.ErrNamespaceCollision, boundary: outputV3AuthorizedMutation, want: outputV3RecoveryAmbiguous},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyOutputV3RecoveryFailure(test.cause, test.boundary); got != test.want {
				t.Fatalf("classifier = %v, want %v", got, test.want)
			}
		})
	}

	pauseErr := recoveryFileOutputFault("recover", denied, outputV3AuthorizedMutation)
	if !outputV3FailureRequiresJobPause(pauseErr) {
		t.Fatalf("operational recovery denial does not require PauseJob: %v", pauseErr)
	}
	ambiguousErr := recoveryFileOutputFault("recover", outputcap.ErrUnsafeNamespace, outputV3AuthorizedMutation)
	if outputV3FailureRequiresJobPause(ambiguousErr) {
		t.Fatalf("unpersisted ambiguity was mislabeled as a retryable pause: %v", ambiguousErr)
	}
}

func outputV3CommitError(transaction *FileTransaction, ctx context.Context) error {
	_, err := transaction.Commit(ctx)
	return err
}

func outputV3RetireError(
	transaction *FileTransaction,
	ctx context.Context,
	reason transfer.FileRetireReason,
) error {
	_, err := transaction.Retire(ctx, reason)
	return err
}

func outputV3PauseError(
	transaction *FileTransaction,
	ctx context.Context,
	reason transfer.FilePauseReason,
) error {
	_, err := transaction.Pause(ctx, reason)
	return err
}

func assertOutputV3Fault(
	t *testing.T,
	err error,
	scope transfer.OutputFaultScope,
	code transfer.OutputFaultCode,
) {
	t.Helper()
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != scope || fault.Code() != code {
		t.Fatalf("output fault = %#v (%v), want scope=%v code=%v", fault, err, scope, code)
	}
}
