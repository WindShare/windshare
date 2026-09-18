package recovery

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type recoveryTestAuthority struct {
	snapshot     Snapshot
	listErr      error
	listCalls    int
	discardCalls int
	discardID    receivecontract.OperationID
	report       DiscardReport
	discardErr   error
}

func (authority *recoveryTestAuthority) List(context.Context) (Snapshot, error) {
	authority.listCalls++
	return authority.snapshot, authority.listErr
}

func (authority *recoveryTestAuthority) Discard(_ context.Context, id receivecontract.OperationID) (DiscardReport, error) {
	authority.discardCalls++
	authority.discardID = id
	return authority.report, authority.discardErr
}

func TestInventoryFreezesSelectionIdentityAndNeverUsesDisplayOrdinals(t *testing.T) {
	first := testResumeOperation("1", OperationResumable)
	second := testResumeOperation("2", OperationIncomplete)
	authority := &recoveryTestAuthority{snapshot: Snapshot{Operations: []Operation{second, first}}}
	inventory, err := Inspect(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inventory.Snapshot()
	if err != nil || snapshot.Operations[0].ID != first.ID {
		t.Fatalf("canonical snapshot=%+v err=%v", snapshot, err)
	}
	// Neither provider-owned slices nor client-edited snapshots can redirect the
	// identity that a previously displayed choice authorizes.
	authority.snapshot.Operations[0].ID = testResumeOperation("3", OperationIncomplete).ID
	snapshot.Operations[0].ID = testResumeOperation("4", OperationIncomplete).ID
	authority.report = DiscardReport{ID: first.ID, Status: Discarded}
	result := inventory.Discard(context.Background(), first.ID)
	if !result.Successful() || authority.discardID != first.ID || authority.discardCalls != 1 {
		t.Fatalf("discard=%+v authority=%+v", result, authority)
	}
	result = inventory.Discard(context.Background(), snapshot.Operations[0].ID)
	if result.Failure == nil || result.Failure.Kind != FailureChanged || authority.discardCalls != 1 {
		t.Fatalf("unlisted identity mutated authority: result=%+v calls=%d", result, authority.discardCalls)
	}
}

func TestDiscardRefusesUnverifiedRegistryAndRunningOperationsBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name            string
		registryUnknown bool
		running         bool
		kind            FailureKind
		reason          string
	}{
		{name: "registry", registryUnknown: true, kind: FailureNeedsAttention, reason: string(AttentionRegistry)},
		{name: "running", running: true, kind: FailureBusy, reason: "operation-already-running"},
	} {
		t.Run(test.name, func(t *testing.T) {
			operation := testResumeOperation("1", OperationIncomplete)
			operation.Running = test.running
			authority := &recoveryTestAuthority{snapshot: Snapshot{
				Operations: []Operation{operation}, RegistryUnknown: test.registryUnknown,
			}}
			inventory, err := Inspect(context.Background(), authority)
			if err != nil {
				t.Fatal(err)
			}
			failure := inventory.CheckDiscard(operation.ID)
			if failure == nil || failure.Kind != test.kind || failure.Reason != test.reason {
				t.Fatalf("decision=%+v", failure)
			}
			result := inventory.Discard(context.Background(), operation.ID)
			if result.Failure == nil || result.Failure.Kind != test.kind || authority.discardCalls != 0 {
				t.Fatalf("result=%+v calls=%d", result, authority.discardCalls)
			}
		})
	}
}

func TestDiscardRecheckClassifiesLeaseRaceDisappearanceAndCancellation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind FailureKind
	}{
		{"busy", osfs.ErrResumeStateBusy, FailureBusy},
		{"disappeared", fs.ErrNotExist, FailureChanged},
		{"cancelled", context.Canceled, FailureCancelled},
		{"deadline", context.DeadlineExceeded, FailureCancelled},
		{"unverified", errors.New("ownership changed"), FailureNeedsAttention},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operation := testResumeOperation("1", OperationIncomplete)
			authority := &recoveryTestAuthority{
				snapshot: Snapshot{Operations: []Operation{operation}}, discardErr: test.err,
			}
			inventory, err := Inspect(context.Background(), authority)
			if err != nil {
				t.Fatal(err)
			}
			result := inventory.Discard(context.Background(), operation.ID)
			if result.Successful() || result.Failure == nil || result.Failure.Kind != test.kind ||
				!errors.Is(result.Err, test.err) || authority.discardCalls != 1 {
				t.Fatalf("rechecked discard=%+v calls=%d", result, authority.discardCalls)
			}
		})
	}
}

func TestDiscardRetainsCleanupDebtAndCompletedCleanupWithCloseFailure(t *testing.T) {
	failure := errors.New("native cleanup or lease close failed")
	for _, test := range []struct {
		name      string
		status    DiscardStatus
		attention AttentionReason
		err       error
		success   bool
	}{
		{"discarded", Discarded, AttentionNone, nil, true},
		{"close failed after discard", Discarded, AttentionNone, failure, false},
		{"cleanup debt", DiscardCleanupPending, AttentionCleanup, failure, false},
		{"cleanup pending without cause", DiscardCleanupPending, AttentionNone, nil, false},
		{"attention", DiscardNeedsAttention, AttentionOperation, failure, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			operation := testResumeOperation("1", OperationIncomplete)
			authority := &recoveryTestAuthority{
				snapshot:   Snapshot{Operations: []Operation{operation}},
				report:     DiscardReport{ID: operation.ID, Status: test.status, Attention: test.attention},
				discardErr: test.err,
			}
			inventory, err := Inspect(context.Background(), authority)
			if err != nil {
				t.Fatal(err)
			}
			result := inventory.Discard(context.Background(), operation.ID)
			if result.Successful() != test.success || result.Report.Status != test.status ||
				result.Report.Attention != test.attention || result.Err != test.err || result.Failure != nil {
				t.Fatalf("settlement=%+v", result)
			}
		})
	}
}

func TestDiscardRejectsAnAuthorityResultForAnotherIdentityAndRetainsItsCause(t *testing.T) {
	operation := testResumeOperation("1", OperationIncomplete)
	cause := errors.New("close failed")
	authority := &recoveryTestAuthority{
		snapshot:   Snapshot{Operations: []Operation{operation}},
		report:     DiscardReport{ID: testResumeOperation("2", OperationIncomplete).ID, Status: Discarded},
		discardErr: cause,
	}
	inventory, err := Inspect(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	result := inventory.Discard(context.Background(), operation.ID)
	if result.Report.Valid() || result.Successful() || !errors.Is(result.Err, ErrContract) ||
		!errors.Is(result.Err, cause) || result.Failure.Kind != FailureNeedsAttention {
		t.Fatalf("mismatched settlement=%+v", result)
	}
}

func TestInspectAndDiscardRejectInvalidOrCancelledBoundaries(t *testing.T) {
	operation := testResumeOperation("1", OperationIncomplete)
	valid := &recoveryTestAuthority{snapshot: Snapshot{Operations: []Operation{operation}}}
	var nilContext context.Context
	if _, err := Inspect(nilContext, valid); !errors.Is(err, ErrContract) {
		t.Fatalf("nil context=%v", err)
	}
	if _, err := Inspect(context.Background(), nil); !errors.Is(err, ErrContract) {
		t.Fatalf("nil authority=%v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Inspect(cancelled, valid); !errors.Is(err, context.Canceled) || valid.listCalls != 0 {
		t.Fatalf("cancelled inventory=%v calls=%d", err, valid.listCalls)
	}
	for _, provider := range []*recoveryTestAuthority{
		{listErr: fs.ErrPermission},
		{snapshot: Snapshot{Operations: []Operation{operation, operation}}},
		{snapshot: Snapshot{Operations: []Operation{{}}}},
	} {
		if _, err := Inspect(context.Background(), provider); err == nil {
			t.Fatal("invalid provider accepted")
		}
	}
	inventory, err := Inspect(context.Background(), valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{nil, cancelled} {
		result := inventory.Discard(ctx, operation.ID)
		if result.Failure == nil || valid.discardCalls != 0 {
			t.Fatalf("invalid discard=%+v calls=%d", result, valid.discardCalls)
		}
	}
	if result := inventory.Discard(context.Background(), receivecontract.OperationID{}); result.Failure == nil {
		t.Fatal("empty identity accepted")
	}
}

func TestSnapshotAndReportCopiesProtectBlockedEvidence(t *testing.T) {
	operation := testResumeOperation("1", OperationIncomplete)
	operation.BlockedItems = []BlockedItem{{ArtifactPath: "tree/file", PathKnown: true, Reason: BlockedRevisionConflict}}
	authority := &recoveryTestAuthority{
		snapshot: Snapshot{Operations: []Operation{operation}},
		report:   DiscardReport{ID: operation.ID, Status: DiscardCleanupPending, BlockedItems: operation.BlockedItems},
	}
	inventory, err := Inspect(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := inventory.Snapshot()
	first.Operations[0].BlockedItems[0].ArtifactPath = "other"
	second, _ := inventory.Snapshot()
	if second.Operations[0].BlockedItems[0].ArtifactPath != "tree/file" {
		t.Fatal("snapshot evidence was mutable")
	}
	result := inventory.Discard(context.Background(), operation.ID)
	authority.report.BlockedItems[0].ArtifactPath = "changed"
	if result.Report.BlockedItems[0].ArtifactPath != "tree/file" {
		t.Fatal("report retained mutable provider evidence")
	}
}
