package resumecommand

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestFilesystemSummaryProjectionUsesClosedOperationVocabulary(t *testing.T) {
	tests := []struct {
		state  osfs.ResumeOperationState
		reason string
		want   resumeOperationState
	}{
		{osfs.ResumeOperationIncomplete, "", resumeOperationIncomplete},
		{osfs.ResumeOperationResumable, "", resumeOperationResumable},
		{osfs.ResumeOperationCleanupPending, "cleanup-uncertain", resumeOperationCleanupPending},
		{osfs.ResumeOperationNeedsAttention, "operation-ownership-unknown", resumeOperationNeedsAttention},
	}
	for _, test := range tests {
		summary := validResumeSummaryView()
		summary.state = test.state
		summary.reason = test.reason
		operation, err := projectResumeStateSummary(summary)
		if err != nil || operation.state != test.want || operation.attention != test.reason {
			t.Fatalf("state=%s operation=%+v err=%v", test.state, operation, err)
		}
	}

	for _, state := range []osfs.ResumeOperationState{0, osfs.ResumeOperationDiscarded} {
		summary := validResumeSummaryView()
		summary.state = state
		if _, err := projectResumeStateSummary(summary); !errors.Is(err, errResumeStateContract) {
			t.Fatalf("state=%d error=%v", state, err)
		}
	}
}

func TestFilesystemSummaryProjectionShowsOnlyBlockedItemsAndHidesControlReferences(t *testing.T) {
	summary := validResumeSummaryView()
	summary.items = []osfs.ResumeStateItem{}
	operation, err := projectResumeStateSummary(summary)
	if err != nil || len(operation.blockedItems) != 0 {
		t.Fatalf("empty projection=%+v err=%v", operation, err)
	}

	blockedTests := []struct {
		item fakeResumeItemView
		want resumeBlockedReason
	}{
		{fakeResumeItemView{path: "result/publish", state: osfs.ResumeItemBlocked, reason: osfs.ResumeItemBlockPublicationUnknown}, resumeBlockedPublicationUnknown},
		{fakeResumeItemView{path: "result/checkpoint", state: osfs.ResumeItemBlocked, reason: osfs.ResumeItemBlockCheckpointInvalid}, resumeBlockedCheckpointInvalid},
		{fakeResumeItemView{path: "result/partial", state: osfs.ResumeItemBlocked, reason: osfs.ResumeItemBlockOwnedObjectUnknown}, resumeBlockedOwnedObjectUnknown},
		{fakeResumeItemView{state: osfs.ResumeItemBlocked, reason: osfs.ResumeItemBlockCheckpointInvalid, reference: "private-record-17"}, resumeBlockedCheckpointInvalid},
	}
	projected := make([]resumeBlockedItem, 0, len(blockedTests))
	for _, test := range blockedTests {
		item, err := projectResumeBlockedItem(test.item)
		if err != nil || item.reason != test.want {
			t.Fatalf("item=%+v projected=%+v err=%v", test.item, item, err)
		}
		projected = append(projected, item)
	}
	operation = testResumeOperation("1", resumeOperationIncomplete)
	operation.blockedItems = projected
	snapshot, err := newResumeInventorySnapshot([]resumeOperation{operation}, false)
	if err != nil {
		t.Fatal(err)
	}
	rendered, _, err := (textRenderer{}).Inventory(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"publication-unknown", "checkpoint-invalid", "owned-object-unknown", "path_known=false"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered=%q missing=%q", rendered, want)
		}
	}
	if strings.Contains(rendered, "private-record-17") {
		t.Fatalf("control reference leaked: %q", rendered)
	}

	for _, invalid := range []fakeResumeItemView{
		{state: osfs.ResumeItemBlocked, reason: osfs.ResumeItemBlockCheckpointInvalid},
		{path: "unsafe/../path", state: osfs.ResumeItemBlocked, reason: osfs.ResumeItemBlockPublicationUnknown},
		{path: "result/file", state: osfs.ResumeItemPublished, reason: osfs.ResumeItemBlockNone},
	} {
		if _, err := projectResumeBlockedItem(invalid); !errors.Is(err, errResumeStateContract) {
			t.Fatalf("invalid=%+v error=%v", invalid, err)
		}
	}
}

func TestResumeInventoryRejectsAmbiguousOrdinalsAndInvalidAttention(t *testing.T) {
	operation := testResumeOperation("1", resumeOperationIncomplete)
	if _, err := newResumeInventorySnapshot([]resumeOperation{operation, operation}, false); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("duplicate error=%v", err)
	}
	invalidAttention := testResumeOperation("2", resumeOperationNeedsAttention)
	invalidAttention.attention = "cleanup-uncertain"
	if _, err := newResumeInventorySnapshot([]resumeOperation{invalidAttention}, false); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("attention error=%v", err)
	}
	unsorted := resumeInventorySnapshot{operations: []resumeOperation{
		testResumeOperation("2", resumeOperationIncomplete),
		testResumeOperation("1", resumeOperationIncomplete),
	}}
	if unsorted.valid() {
		t.Fatal("unsorted ordinal snapshot was accepted")
	}
	if _, _, err := (textRenderer{}).Inventory(unsorted); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("render error=%v", err)
	}
}

func TestFilesystemDiscardProjectionSeparatesCommandOutcomeFromInventoryHistory(t *testing.T) {
	tests := []struct {
		state  osfs.ResumeOperationState
		reason string
		want   string
	}{
		{osfs.ResumeOperationDiscarded, "", resumeDiscardStatusDiscarded},
		{osfs.ResumeOperationCleanupPending, "cleanup-uncertain", resumeDiscardStatusCleanupPending},
		{osfs.ResumeOperationNeedsAttention, "operation-ownership-unknown", resumeDiscardStatusNeedsAttention},
	}
	for _, test := range tests {
		summary := validResumeSummaryView()
		summary.state = test.state
		summary.reason = test.reason
		report, err := projectResumeDiscardSummary(summary)
		if err != nil || report.status != test.want || !report.valid() {
			t.Fatalf("state=%d report=%+v err=%v", test.state, report, err)
		}
	}
	summary := validResumeSummaryView()
	if _, err := projectResumeDiscardSummary(summary); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("active discard report error=%v", err)
	}
}

func TestFilesystemInventoryAndDiscardFailClosedForDetachedValues(t *testing.T) {
	var detached *filesystemResumeStateInventory
	if _, err := detached.Snapshot(); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("detached snapshot error=%v", err)
	}
	if _, err := detached.Discard(context.Background(), 0); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("detached discard error=%v", err)
	}
	if _, err := decodeResumeOperationID("not-hex"); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("decode error=%v", err)
	}
	if _, err := projectResumeStateSummary((*fakeResumeSummaryView)(nil)); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("nil summary error=%v", err)
	}
}

func TestFilesystemRunnerHelpUsesOnlyRootOwnedOperationInventory(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runner := NewFilesystemRunner(FilesystemConfig{
		Input: strings.NewReader(""), Output: stdout,
		RawTerminalOutput: stderr, SerializedTerminalOutput: stderr,
	})
	if result := runner.Run(context.Background(), []string{"help"}); result != ResultOK {
		t.Fatalf("result=%d", result)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "resume list -o") ||
		!strings.Contains(stderr.String(), "resume discard -o") ||
		strings.Contains(stderr.String(), "resume cleanup") || strings.Contains(stderr.String(), "legacy") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

type fakeResumeSummaryView struct {
	operation  receivecontract.OperationID
	intent     transfer.ReceiveIntentDigest
	state      osfs.ResumeOperationState
	generation uint64
	reason     string
	items      []osfs.ResumeStateItem
	busy       bool
	valid      bool
}

func validResumeSummaryView() *fakeResumeSummaryView {
	operation, _ := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{0x11}, receivecontract.StableIdentityBytes))
	intent, _ := transfer.ReceiveIntentDigestFromBytes(bytes.Repeat([]byte{0x22}, transfer.ReceiveIntentDigestBytes))
	return &fakeResumeSummaryView{
		operation: operation, intent: intent, state: osfs.ResumeOperationIncomplete,
		generation: 1, valid: true,
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
func (summary *fakeResumeSummaryView) NeedsAttentionReason() string {
	if summary == nil {
		return ""
	}
	return summary.reason
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
	reason    osfs.ResumeItemBlockReason
	reference string
}

func (item fakeResumeItemView) CanonicalPath() string                   { return item.path }
func (item fakeResumeItemView) State() osfs.ResumeItemState             { return item.state }
func (item fakeResumeItemView) BlockReason() osfs.ResumeItemBlockReason { return item.reason }
func (item fakeResumeItemView) DiagnosticReference() string             { return item.reference }

var _ resumeSummaryView = (*fakeResumeSummaryView)(nil)
var _ resumeItemView = fakeResumeItemView{}
