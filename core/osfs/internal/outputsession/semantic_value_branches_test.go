package outputsession

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestSessionAndClaimAccessorsExposeOnlyReservedSemanticCoordinates(t *testing.T) {
	fixture := newTestFixture(t, nil)
	if fixture.session.Binding() == (transfer.DirectTreeSessionBinding{}) ||
		fixture.session.SessionID() != fixture.sessionID ||
		fixture.session.Capabilities().Durability != transfer.DurabilityPowerLoss {
		t.Fatal("session accessors lost frozen output authority")
	}
	var nilSession *Session
	if nilSession.Binding() != (transfer.DirectTreeSessionBinding{}) ||
		!nilSession.SessionID().IsZero() || nilSession.Capabilities() != (transfer.DirectTreeCapabilities{}) {
		t.Fatal("nil session accessors manufactured authority")
	}

	rootAdmission := fixture.admitRoot(context.Background())
	fixture.session.mu.Lock()
	rootID := fixture.session.receiptClaims[receiptKey(rootAdmission)]
	rootEntry := fixture.session.directoryClaims[rootID]
	rootClaim := rootEntry.claim
	fixture.session.mu.Unlock()
	if rootClaim.ID() != rootID || rootClaim.Admission().IsZero() ||
		rootClaim.Source().DirectoryID.IsZero() || !rootClaim.ArtifactPath().Valid() ||
		rootClaim.LocatorKey() == "" || !rootClaim.DestinationPath().Valid() ||
		rootClaim.DestinationLocatorKey() == "" || rootClaim.ParentID() != 0 ||
		rootClaim.SourceParentID() != 0 || !rootClaim.IsSessionRoot() {
		t.Fatalf("root claim = %+v", rootClaim)
	}

	file := fixture.outputFile(rootAdmission, 0x71, "file.bin")
	start, err := fixture.session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	fixture.session.mu.Lock()
	var fileClaim FileClaim
	for _, entry := range fixture.session.fileClaims {
		fileClaim = entry.claim
		break
	}
	fixture.session.mu.Unlock()
	if fileClaim.ID() == 0 || fileClaim.File().Descriptor() != file.Descriptor() ||
		fileClaim.LocatorKey() == "" || !fileClaim.DestinationPath().Valid() ||
		fileClaim.DestinationLocatorKey() == "" || fileClaim.ParentID() != rootID {
		t.Fatalf("file claim = %+v", fileClaim)
	}
	transaction, _, ok := start.Transaction()
	if !ok || transaction.Binding() == (transfer.MaterializedFileBinding{}) {
		t.Fatal("file start omitted bound transaction")
	}
	if (&guardedTransaction{}).Binding() != (transfer.MaterializedFileBinding{}) {
		t.Fatal("empty guarded transaction manufactured a binding")
	}
	if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
}

func TestOutputSessionClosedProjectionAndBranchVocabulary(t *testing.T) {
	fixture := newTestFixture(t, nil)
	root := fixture.admitRoot(context.Background())
	file := fixture.outputFile(root, 0x72, "dir/file.bin")
	if !transfer.MaterializationFileMatchesProjector(fixture.session.projector, file) ||
		file.SourcePath().String() != "dir/file.bin" ||
		file.ArtifactPath().String() != receivecontract.DefaultResultRootName+"/dir/file.bin" {
		t.Fatalf("closed file projection = (%q, %q)", file.SourcePath().String(), file.ArtifactPath().String())
	}
	if transfer.MaterializationFileMatchesProjector(
		fixture.session.projector, transfer.MaterializationFile{},
	) {
		t.Fatal("zero file request matched the frozen projector")
	}
	if _, err := fixture.session.BeginFile(
		context.Background(), transfer.MaterializationFile{},
	); !errors.Is(err, ErrDirectoryBinding) {
		t.Fatalf("zero file request = %v", err)
	}

	traverse, materialize, err := directoryProjection(ordinaryoutput.TraverseOnlyProjection())
	if err != nil || materialize || traverse.Valid() {
		t.Fatalf("traverse projection = (%+v, %t, %v)", traverse, materialize, err)
	}
	artifact := mustArtifactPath(t, "artifact")
	projection, _ := ordinaryoutput.MaterializeArtifactProjection(artifact)
	projected, materialize, err := directoryProjection(projection)
	if err != nil || !materialize || projected != artifact {
		t.Fatalf("materialized projection = (%+v, %t, %v)", projected, materialize, err)
	}
	if _, _, err := directoryProjection(ordinaryoutput.ArtifactPathProjection{}); !errors.Is(err, ErrDirectoryBinding) {
		t.Fatalf("invalid projection = %v", err)
	}
	if _, err := ArtifactDestinationBinderFunc(nil).BindArtifactPath(artifact); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil destination binder = %v", err)
	}
	if err := ResourceReleaserFunc(nil).ReleaseOutputSession(context.Background()); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil resource releaser = %v", err)
	}
	called := false
	if err := ResourceReleaserFunc(func(context.Context) error {
		called = true
		return nil
	}).ReleaseOutputSession(context.Background()); err != nil || !called {
		t.Fatalf("resource releaser = (called %t, %v)", called, err)
	}

	pauseReasons := map[transfer.JobPauseReason]transfer.FilePauseReason{
		transfer.JobPauseInterrupted:      transfer.FilePauseInterrupted,
		transfer.JobPauseShutdown:         transfer.FilePauseShutdown,
		transfer.JobPauseTransportFailure: transfer.FilePauseTransportFailure,
		transfer.JobPauseSessionFailure:   transfer.FilePauseSessionFailure,
		transfer.JobPauseOutputFailure:    transfer.FilePauseOutputFailure,
		transfer.JobPauseResourceBudget:   transfer.FilePauseResourceBudget,
		0:                                 transfer.FilePauseDependencyContract,
	}
	for input, want := range pauseReasons {
		if got := filePauseReasonForJob(input); got != want {
			t.Fatalf("pause reason %d = %d, want %d", input, got, want)
		}
	}
	for state, want := range map[directoryState]ClaimState{
		directoryPending: ClaimPending, directoryAdmitted: ClaimAdmitted,
		directorySettling: ClaimSettling, directorySettled: ClaimSettled,
		0: 0,
	} {
		if got := directoryClaimState(state); got != want {
			t.Fatalf("directory state %d = %d", state, got)
		}
	}
	for state, want := range map[fileState]ClaimState{
		filePending: ClaimPending, fileActive: ClaimActive, fileSettled: ClaimSettled, 0: 0,
	} {
		if got := fileClaimState(state); got != want {
			t.Fatalf("file state %d = %d", state, got)
		}
	}
	if err := joinCompletedFailures(nil, nil); err != nil {
		t.Fatalf("empty completed failures = %v", err)
	}
	if err := joinCompletedFailures(errors.New("first"), errors.New("second")); err == nil {
		t.Fatal("completed failures were discarded")
	}
}

func TestOutputSessionIdentityExhaustionRequiresPause(t *testing.T) {
	fixture := newTestFixture(t, nil)
	fixture.session.mu.Lock()
	fixture.session.nextOperationID = math.MaxUint64
	if _, err := fixture.session.nextOperationLocked(); err == nil {
		fixture.session.mu.Unlock()
		t.Fatal("operation identity exhaustion succeeded")
	}
	fixture.session.nextClaimID = ClaimID(math.MaxUint64)
	if _, err := fixture.session.nextClaimLocked(); err == nil {
		fixture.session.mu.Unlock()
		t.Fatal("claim identity exhaustion succeeded")
	}
	if err := fixture.session.operationRejectionOrInvariantLocked(); err == nil {
		fixture.session.mu.Unlock()
		t.Fatal("required-pause session accepted another operation")
	}
	fixture.session.mu.Unlock()
	if _, err := fixture.session.PauseTree(context.Background(), transfer.JobPauseOutputFailure); err != nil {
		t.Fatal(err)
	}
}
