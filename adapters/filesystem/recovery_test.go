package filesystem

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/recovery"
	"github.com/windshare/windshare/osfs"
)

func TestFilesystemSummaryProjectionUsesClosedOperationVocabulary(t *testing.T) {
	tests := []struct {
		State  osfs.ResumeOperationState
		Reason osfs.FilesystemOutputStateReason
		want   recovery.OperationState
	}{
		{osfs.ResumeOperationIncomplete, osfs.FilesystemOutputStateReasonNone, recovery.OperationIncomplete},
		{osfs.ResumeOperationResumable, osfs.FilesystemOutputStateReasonNone, recovery.OperationResumable},
		{osfs.ResumeOperationCleanupPending, osfs.FilesystemOutputStateCleanupUncertain, recovery.OperationCleanupPending},
		{osfs.ResumeOperationNeedsAttention, osfs.FilesystemOutputStateOperationOwnershipUnknown, recovery.OperationNeedsAttention},
	}
	for _, test := range tests {
		summary := validResumeSummaryView()
		summary.state = test.State
		summary.Reason = test.Reason
		operation, err := projectStateSummary(summary)
		wantAttention := recovery.AttentionNone
		if test.Reason != osfs.FilesystemOutputStateReasonNone {
			wantAttention = recovery.AttentionReason(test.Reason.String())
		}
		if err != nil || operation.State != test.want || operation.Attention != wantAttention {
			t.Fatalf("State=%s operation=%+v err=%v", test.State, operation, err)
		}
	}

	for _, State := range []osfs.ResumeOperationState{0, osfs.ResumeOperationDiscarded} {
		summary := validResumeSummaryView()
		summary.state = State
		if _, err := projectStateSummary(summary); !errors.Is(err, recovery.ErrContract) {
			t.Fatalf("State=%d error=%v", State, err)
		}
	}
}

func TestFilesystemSummaryProjectionShowsOnlyBlockedItemsAndHidesControlReferences(t *testing.T) {
	summary := validResumeSummaryView()
	summary.items = []osfs.ResumeStateItem{}
	operation, err := projectStateSummary(summary)
	if err != nil || len(operation.BlockedItems) != 0 {
		t.Fatalf("empty projection=%+v err=%v", operation, err)
	}

	blockedTests := []struct {
		item fakeResumeItemView
		want recovery.BlockedReason
	}{
		{fakeResumeItemView{path: "result/publish", state: osfs.ResumeItemBlocked, Reason: osfs.ResumeItemBlockPublicationUnknown}, recovery.BlockedPublicationUnknown},
		{fakeResumeItemView{path: "result/checkpoint", state: osfs.ResumeItemBlocked, Reason: osfs.ResumeItemBlockCheckpointInvalid}, recovery.BlockedCheckpointInvalid},
		{fakeResumeItemView{path: "result/partial", state: osfs.ResumeItemBlocked, Reason: osfs.ResumeItemBlockOwnedObjectUnknown}, recovery.BlockedOwnedObjectUnknown},
		{fakeResumeItemView{path: "result/revision", state: osfs.ResumeItemBlocked, Reason: osfs.ResumeItemBlockRevisionConflict}, recovery.BlockedRevisionConflict},
		{fakeResumeItemView{state: osfs.ResumeItemBlocked, Reason: osfs.ResumeItemBlockCheckpointInvalid, reference: "private-record-17"}, recovery.BlockedCheckpointInvalid},
	}
	projected := make([]recovery.BlockedItem, 0, len(blockedTests))
	for _, test := range blockedTests {
		item, err := projectBlockedItem(test.item)
		if err != nil || item.Reason != test.want {
			t.Fatalf("item=%+v projected=%+v err=%v", test.item, item, err)
		}
		projected = append(projected, item)
	}
	operation = testResumeOperation("1", recovery.OperationIncomplete)
	operation.BlockedItems = projected
	snapshot, err := recovery.NewSnapshot([]recovery.Operation{operation}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Operations[0].BlockedItems) != len(blockedTests) {
		t.Fatal("blocked recovery evidence was lost")
	}
	if item := snapshot.Operations[0].BlockedItems[len(blockedTests)-1]; item.PathKnown || item.ArtifactPath != "" {
		t.Fatalf("control reference was projected as an artifact: %+v", item)
	}

	for _, invalid := range []fakeResumeItemView{
		{state: osfs.ResumeItemBlocked, Reason: osfs.ResumeItemBlockCheckpointInvalid},
		{path: "unsafe/../path", state: osfs.ResumeItemBlocked, Reason: osfs.ResumeItemBlockPublicationUnknown},
		{path: "result/file", state: osfs.ResumeItemPublished, Reason: osfs.ResumeItemBlockNone},
	} {
		if _, err := projectBlockedItem(invalid); !errors.Is(err, recovery.ErrContract) {
			t.Fatalf("invalid=%+v error=%v", invalid, err)
		}
	}
}

func TestResumeInventoryRejectsAmbiguousOrdinalsAndInvalidAttention(t *testing.T) {
	operation := testResumeOperation("1", recovery.OperationIncomplete)
	if _, err := recovery.NewSnapshot([]recovery.Operation{operation, operation}, false); !errors.Is(err, recovery.ErrContract) {
		t.Fatalf("duplicate error=%v", err)
	}
	invalidAttention := testResumeOperation("2", recovery.OperationNeedsAttention)
	invalidAttention.Attention = "cleanup-uncertain"
	if _, err := recovery.NewSnapshot([]recovery.Operation{invalidAttention}, false); !errors.Is(err, recovery.ErrContract) {
		t.Fatalf("Attention error=%v", err)
	}
	unsorted := recovery.Snapshot{Operations: []recovery.Operation{
		testResumeOperation("2", recovery.OperationIncomplete),
		testResumeOperation("1", recovery.OperationIncomplete),
	}}
	if unsorted.Valid() {
		t.Fatal("unsorted ordinal snapshot was accepted")
	}

}

func TestFilesystemDiscardProjectionSeparatesCommandOutcomeFromInventoryHistory(t *testing.T) {
	tests := []struct {
		State  osfs.ResumeOperationState
		Reason osfs.FilesystemOutputStateReason
		want   recovery.DiscardStatus
	}{
		{osfs.ResumeOperationDiscarded, osfs.FilesystemOutputStateReasonNone, recovery.Discarded},
		{osfs.ResumeOperationCleanupPending, osfs.FilesystemOutputStateCleanupUncertain, recovery.DiscardCleanupPending},
		{osfs.ResumeOperationNeedsAttention, osfs.FilesystemOutputStateOperationOwnershipUnknown, recovery.DiscardNeedsAttention},
	}
	for _, test := range tests {
		summary := validResumeSummaryView()
		summary.state = test.State
		summary.Reason = test.Reason
		report, err := projectDiscardSummary(summary)
		if err != nil || report.Status != test.want || !report.Valid() {
			t.Fatalf("State=%d report=%+v err=%v", test.State, report, err)
		}
	}
	summary := validResumeSummaryView()
	if _, err := projectDiscardSummary(summary); !errors.Is(err, recovery.ErrContract) {
		t.Fatalf("active discard report error=%v", err)
	}
}

func TestInventoryAndDiscardFailClosedForDetachedValues(t *testing.T) {
	var detached *recovery.Inventory
	if _, err := detached.Snapshot(); !errors.Is(err, recovery.ErrContract) {
		t.Fatalf("detached snapshot error=%v", err)
	}
	result := detached.Discard(context.Background(), receivecontract.OperationID{})
	if !errors.Is(result.Err, recovery.ErrContract) || result.Successful() {
		t.Fatalf("detached discard=%+v", result)
	}
	if _, err := projectStateSummary((*fakeResumeSummaryView)(nil)); !errors.Is(err, recovery.ErrContract) {
		t.Fatalf("nil summary error=%v", err)
	}
}

type fakeResumeSummaryView struct {
	operation  receivecontract.OperationID
	intent     transfer.ReceiveIntentDigest
	state      osfs.ResumeOperationState
	generation uint64
	Reason     osfs.FilesystemOutputStateReason
	items      []osfs.ResumeStateItem
	busy       bool
	valid      bool
}

func validResumeSummaryView() *fakeResumeSummaryView {
	operation, _ := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{0x11}, receivecontract.StableIdentityBytes))
	intent, _ := transfer.ReceiveIntentDigestFromBytes(bytes.Repeat([]byte{0x22}, transfer.ReceiveIntentDigestBytes))
	return &fakeResumeSummaryView{
		operation: operation, intent: intent, state: osfs.ResumeOperationIncomplete,
		generation: 1, Reason: osfs.FilesystemOutputStateReasonNone, valid: true,
	}
}

func (summary *fakeResumeSummaryView) OperationID() receivecontract.OperationID {
	if summary == nil {
		return receivecontract.OperationID{}
	}
	return summary.operation
}
func (summary *fakeResumeSummaryView) ReceiveIntentDigest() transfer.ReceiveIntentDigest {
	if summary == nil {
		return transfer.ReceiveIntentDigest{}
	}
	return summary.intent
}
func (summary *fakeResumeSummaryView) State() osfs.ResumeOperationState {
	if summary == nil {
		return 0
	}
	return summary.state
}
func (summary *fakeResumeSummaryView) StateGeneration() uint64 {
	if summary == nil {
		return 0
	}
	return summary.generation
}
func (summary *fakeResumeSummaryView) NeedsAttentionReason() osfs.FilesystemOutputStateReason {
	if summary == nil {
		return 0
	}
	return summary.Reason
}
func (summary *fakeResumeSummaryView) Items() []osfs.ResumeStateItem {
	if summary == nil {
		return nil
	}
	return append([]osfs.ResumeStateItem(nil), summary.items...)
}
func (summary *fakeResumeSummaryView) Busy() bool {
	return summary != nil && summary.busy
}
func (summary *fakeResumeSummaryView) Valid() bool {
	return summary != nil && summary.valid
}

type fakeResumeItemView struct {
	path      string
	state     osfs.ResumeItemState
	Reason    osfs.ResumeItemBlockReason
	reference string
}

func (item fakeResumeItemView) CanonicalPath() string                   { return item.path }
func (item fakeResumeItemView) State() osfs.ResumeItemState             { return item.state }
func (item fakeResumeItemView) BlockReason() osfs.ResumeItemBlockReason { return item.Reason }
func (item fakeResumeItemView) DiagnosticReference() string             { return item.reference }

var _ nativeSummaryView = (*fakeResumeSummaryView)(nil)
var _ nativeItemView = fakeResumeItemView{}

func testResumeOperation(fill string, state recovery.OperationState) recovery.Operation {
	decoded, _ := hex.DecodeString(strings.Repeat(fill, 32))
	id, _ := receivecontract.OperationIDFromBytes(decoded)
	return recovery.Operation{ID: id, State: state}
}
