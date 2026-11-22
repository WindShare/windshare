//go:build windows || linux || darwin

package osfs

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/output"
)

const retainedCandidateExactSize = 8

type retainedCandidateFixture struct {
	root     string
	intent   transfer.ReceiveIntent
	rootSeed byte
	fileSeed byte
}

func TestNativeOutputRetainedCandidateListAndReopen(t *testing.T) {
	listFixture := newRetainedCandidateFixture(t, "list-from-torn-tail", 0x71)
	listResume := listRetainedCandidate(t, listFixture)
	discardRetainedCandidate(t, listResume, listFixture.intent.OperationID())

	getFixture := newRetainedCandidateFixture(t, "get-from-torn-tail", 0x81)
	reopenRetainedCandidateOutput(t, getFixture, false)
	reopenRetainedCandidateOutput(t, getFixture, true)

	sharedFixture := newRetainedCandidateFixture(t, "list-then-get", 0x91)
	sharedResume := listRetainedCandidate(t, sharedFixture)
	reopenRetainedCandidateOutput(t, sharedFixture, true)
	discardRetainedCandidate(t, sharedResume, sharedFixture.intent.OperationID())
}

func newRetainedCandidateFixture(t *testing.T, name string, seed byte) retainedCandidateFixture {
	t.Helper()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), name)
	authority, operation, intent := openPublicOrdinaryOperation(t, root, seed)
	session, err := operation.OpenDirectTree(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	rootSeed := seed + 1
	fileSeed := seed + 2
	rootAdmission := admitRetainedCandidateRoot(t, session, intent, rootSeed)
	file := nativeDirectTreeTestFile(
		t, session, intent, fileSeed, "candidate.bin", retainedCandidateExactSize, rootAdmission,
	)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatal("retained candidate file settled before receiving content")
	}
	if err := transaction.WriteRange(ctx, 0, []byte("part")); err != nil {
		t.Fatal(err)
	}
	durable, err := transaction.Checkpoint(ctx)
	if err != nil || len(durable.Ranges().Ranges()) != 1 {
		t.Fatalf("initial checkpoint = (%v, %v)", durable.Ranges().Ranges(), err)
	}
	if settlement, err := transaction.Pause(ctx, transfer.FilePauseInterrupted); err != nil ||
		settlement.Kind() != transfer.FilePaused {
		t.Fatalf("initial file pause = (%d, %v)", settlement.Kind(), err)
	}
	if settlement, err := session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil ||
		settlement.Kind() != transfer.DirectTreeSettlementPaused {
		t.Fatalf("initial tree pause = (%d, %v)", settlement.Kind(), err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate an interrupted trailing journal append after a valid checkpoint.
	journalPath := filepath.Join(root, output.ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()), "state.log")
	f, err := os.OpenFile(journalPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("deadbeef {\"k\":\"partial\",\"truncated"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	return retainedCandidateFixture{
		root: root, intent: intent, rootSeed: rootSeed, fileSeed: fileSeed,
	}
}

func listRetainedCandidate(t *testing.T, fixture retainedCandidateFixture) *RecoveryAuthority {
	t.Helper()
	resume := NewRecoveryAuthority(fixture.root)
	inventory, err := resume.List(context.Background())
	summaries := inventory.Summaries()
	if err != nil || inventory.Status() != RecoveryListReady ||
		len(summaries) != 1 || summaries[0].OperationID() != fixture.intent.OperationID() ||
		summaries[0].State() != RecoveryOperationResumable ||
		summaries[0].ReceiveIntentDigest() != fixture.intent.Digest() {
		t.Fatalf("retained candidate inventory = (%+v, %v)", inventory, err)
	}
	return resume
}

func reopenRetainedCandidateOutput(t *testing.T, fixture retainedCandidateFixture, continueFile bool) {
	t.Helper()
	ctx := context.Background()
	reopened, err := NewOutputAuthority(OutputConfig{RootPath: fixture.root})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.BindDestination(ctx); err != nil {
		t.Fatalf("reopen bind = %v", err)
	}
	lookup, err := reopened.SelectOperation(ctx, fixture.intent.SelectionSpec(), receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != OutputLookupReopened || lookup.Attention() != OutputAttentionNone {
		t.Fatalf("retained candidate lookup = (%d, %s, %v)", lookup.Kind(), lookup.Attention(), err)
	}
	reopenedIntent, present := lookup.Operation().ReceiveIntent()
	if !present || !reopenedIntent.EqualCanonical(fixture.intent) {
		t.Fatalf("retained candidate operation identity = (%+v, %t)", reopenedIntent, present)
	}
	resumedSession, err := lookup.Operation().OpenDirectTree(ctx, reopenedIntent)
	if err != nil {
		t.Fatal(err)
	}
	if !continueFile {
		if settlement, pauseErr := resumedSession.PauseTree(ctx, transfer.JobPauseInterrupted); pauseErr != nil ||
			settlement.Kind() != transfer.DirectTreeSettlementPaused {
			t.Fatalf("tree pause = (%d, %v)", settlement.Kind(), pauseErr)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}

	resumedRoot := admitRetainedCandidateRoot(t, resumedSession, fixture.intent, fixture.rootSeed)
	resumedFile := nativeDirectTreeTestFile(
		t, resumedSession, fixture.intent, fixture.fileSeed,
		"candidate.bin", retainedCandidateExactSize, resumedRoot,
	)
	resumedStart, err := resumedSession.BeginFile(ctx, resumedFile)
	if err != nil {
		t.Fatal(err)
	}
	resumedTransaction, resumedDurable, ok := resumedStart.Transaction()
	ranges := resumedDurable.Ranges().Ranges()
	if !ok || len(ranges) != 1 || ranges[0].Offset != 0 || ranges[0].End != 4 {
		t.Fatalf("resumed durable ranges = (%v, transaction=%t)", ranges, ok)
	}
	if err := resumedTransaction.WriteRange(ctx, 4, []byte("done")); err != nil {
		t.Fatal(err)
	}
	if _, err := resumedTransaction.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if settlement, err := resumedTransaction.Pause(ctx, transfer.FilePauseInterrupted); err != nil ||
		settlement.Kind() != transfer.FilePaused {
		t.Fatalf("resumed file pause = (%d, %v)", settlement.Kind(), err)
	}
	if settlement, err := resumedSession.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil ||
		settlement.Kind() != transfer.DirectTreeSettlementPaused {
		t.Fatalf("resumed tree pause = (%d, %v)", settlement.Kind(), err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func discardRetainedCandidate(t *testing.T, resume *RecoveryAuthority, operation receivecontract.OperationID) {
	t.Helper()
	exact, err := resume.List(context.Background())
	exactSummaries := exact.Summaries()
	if err != nil || len(exactSummaries) != 1 ||
		exactSummaries[0].OperationID() != operation || exactSummaries[0].State() != RecoveryOperationResumable {
		t.Fatalf("exact pre-discard snapshot = (%+v, %v)", exact, err)
	}
	discarded, err := resume.Discard(context.Background(), operation)
	if err != nil || discarded.State() != RecoveryOperationDiscarded || discarded.OperationID() != operation {
		t.Fatalf("exact discard = (%+v, %v)", discarded, err)
	}
}

func admitRetainedCandidateRoot(
	t *testing.T,
	session transfer.DirectTreeSession,
	intent transfer.ReceiveIntent,
	seed byte,
) transfer.DirectoryAdmission {
	t.Helper()
	request, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: intent.SyntheticRoot(),
			Generation:  coverageC6Identity[catalog.DirectoryGeneration](seed),
			SourcePath:  ordinaryoutput.EmptySourceCatalogPath(),
		},
		ordinaryoutput.SourceNodeSelected,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := session.AdmitDirectory(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return admission
}
