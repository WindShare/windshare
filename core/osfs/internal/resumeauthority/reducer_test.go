package resumeauthority

import (
	"bytes"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type ordinaryResumeFixture struct {
	record checkpointmodel.OrdinaryOperationRecord
	intent transfer.ReceiveIntent
}

func newOrdinaryResumeFixture(t *testing.T, seed byte) ordinaryResumeFixture {
	t.Helper()
	var share catalog.ShareInstance
	var root catalog.DirectoryID
	share[0], root[0] = seed, seed+1
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operation, _ := receivecontract.OperationIDFromBytes(
		bytes.Repeat([]byte{seed + 2}, receivecontract.StableIdentityBytes),
	)
	reservationID, _ := receivecontract.DestinationReservationIDFromBytes(
		bytes.Repeat([]byte{seed + 3}, receivecontract.StableIdentityBytes),
	)
	authority, _ := receivecontract.AuthorityRefFromBytes(
		bytes.Repeat([]byte{seed + 4}, receivecontract.AuthorityRefBytes),
	)
	reservation, err := receivecontract.NewNativeContainerRootReservation(
		operation, reservationID, artifact, authority,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	key, err := checkpointmodel.NewActiveOperationKeyV1(selection.Digest(), authority)
	if err != nil {
		t.Fatal(err)
	}
	var token [32]byte
	token[0] = seed + 5
	claim, err := checkpointmodel.NewReservationClaimLocator(token, 4)
	if err != nil {
		t.Fatal(err)
	}
	record, err := checkpointmodel.NewOrdinaryOperationRecord(
		checkpointmodel.OrdinaryOperationRecordSpec{
			ActiveKey: key, Intent: intent, ReservationClaim: claim,
			LifecycleGeneration: 1, Lifecycle: checkpointmodel.OrdinaryOperationActive,
			Lease:        checkpointmodel.OrdinaryLeaseReleased,
			ClosedReason: checkpointmodel.OrdinaryReasonNone,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return ordinaryResumeFixture{record: record, intent: intent}
}

func resumeRecordState(
	t *testing.T,
	record checkpointmodel.OrdinaryOperationRecord,
	state checkpointmodel.OrdinaryOperationLifecycle,
	reason checkpointmodel.OrdinaryClosedReason,
) checkpointmodel.OrdinaryOperationRecord {
	t.Helper()
	next, err := checkpointmodel.NextOrdinaryOperationRecord(
		record,
		checkpointmodel.NextOrdinaryOperationRecordSpec{
			Lifecycle: state, Lease: checkpointmodel.OrdinaryLeaseReleased,
			ClosedReason: reason,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func resumeHeader(t *testing.T, record checkpointmodel.OrdinaryOperationRecord) Header {
	t.Helper()
	header, err := NewHeader(record)
	if err != nil {
		t.Fatal(err)
	}
	return header
}

func resumeItem(t *testing.T, path string, state ItemState) Item {
	t.Helper()
	item, err := NewItem(path, state, ItemBlockNone)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestRecoveryReducerKeepsItemFailuresBelowOperationAuthority(t *testing.T) {
	fixture := newOrdinaryResumeFixture(t, 0x21)
	header := resumeHeader(t, fixture.record)
	blocked, err := NewItem("result/blocked", ItemBlocked, ItemBlockPublicationUnknown)
	if err != nil {
		t.Fatal(err)
	}
	incomplete := resumeItem(t, "result/new", ItemIncomplete)
	resumable := resumeItem(t, "result/partial", ItemResumable)

	for name, test := range map[string]struct {
		items []Item
		want  OperationState
	}{
		"no checkpoint": {want: OperationIncomplete},
		"failed item remains active": {
			items: []Item{blocked, incomplete}, want: OperationIncomplete,
		},
		"one exact partial makes operation resumable": {
			items: []Item{blocked, resumable}, want: OperationResumable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot, err := NewSnapshot(header, test.items)
			if err != nil {
				t.Fatal(err)
			}
			summary, err := ReduceRecovery(snapshot)
			if err != nil || summary.State() != test.want ||
				len(summary.Items()) != len(test.items) {
				t.Fatalf("summary = %+v, %v", summary, err)
			}
		})
	}
}

func TestRecoveryReducerProjectsOnlyFiveOrdinaryStates(t *testing.T) {
	fixture := newOrdinaryResumeFixture(t, 0x31)
	tests := []struct {
		lifecycle checkpointmodel.OrdinaryOperationLifecycle
		reason    checkpointmodel.OrdinaryClosedReason
		want      OperationState
	}{
		{checkpointmodel.OrdinaryOperationActive, checkpointmodel.OrdinaryReasonNone, OperationIncomplete},
		{checkpointmodel.OrdinaryOperationNeedsAttention, checkpointmodel.OrdinaryReasonOperationOwnershipUnknown, OperationNeedsAttention},
		{checkpointmodel.OrdinaryOperationCompleted, checkpointmodel.OrdinaryReasonNone, OperationCleanupPending},
		{checkpointmodel.OrdinaryOperationDiscarded, checkpointmodel.OrdinaryReasonNone, OperationCleanupPending},
		{checkpointmodel.OrdinaryOperationCleanupPending, checkpointmodel.OrdinaryReasonCleanupUncertain, OperationCleanupPending},
	}
	for _, test := range tests {
		record := fixture.record
		if test.lifecycle != checkpointmodel.OrdinaryOperationActive {
			record = resumeRecordState(t, record, test.lifecycle, test.reason)
		}
		snapshot, err := NewSnapshot(resumeHeader(t, record), nil)
		if err != nil {
			t.Fatal(err)
		}
		summary, err := ReduceRecovery(snapshot)
		if err != nil || summary.State() != test.want {
			t.Fatalf("%s => %s, %v", test.lifecycle.String(), summary.State(), err)
		}
	}
}

func TestDiscardReducerDeindexesBeforeCleanup(t *testing.T) {
	fixture := newOrdinaryResumeFixture(t, 0x41)
	for _, lifecycle := range []checkpointmodel.OrdinaryOperationLifecycle{
		checkpointmodel.OrdinaryOperationActive,
		checkpointmodel.OrdinaryOperationNeedsAttention,
		checkpointmodel.OrdinaryOperationCompleted,
		checkpointmodel.OrdinaryOperationDiscarded,
		checkpointmodel.OrdinaryOperationCleanupPending,
	} {
		record := fixture.record
		switch lifecycle {
		case checkpointmodel.OrdinaryOperationNeedsAttention:
			record = resumeRecordState(t, record, lifecycle, checkpointmodel.OrdinaryReasonOperationOwnershipUnknown)
		case checkpointmodel.OrdinaryOperationCompleted,
			checkpointmodel.OrdinaryOperationDiscarded:
			record = resumeRecordState(t, record, lifecycle, checkpointmodel.OrdinaryReasonNone)
		case checkpointmodel.OrdinaryOperationCleanupPending:
			completed := resumeRecordState(t, record, checkpointmodel.OrdinaryOperationCompleted, checkpointmodel.OrdinaryReasonNone)
			record = resumeRecordState(t, completed, lifecycle, checkpointmodel.OrdinaryReasonCleanupUncertain)
		}
		action, err := ReduceDiscard(record)
		want := DiscardCleanup
		if lifecycle == checkpointmodel.OrdinaryOperationActive ||
			lifecycle == checkpointmodel.OrdinaryOperationNeedsAttention {
			want = DiscardTransitionAndCleanup
		}
		if err != nil || action != want {
			t.Fatalf("%s => %d, %v", lifecycle.String(), action, err)
		}
	}
	if _, err := ReduceDiscard(checkpointmodel.OrdinaryOperationRecord{}); err == nil {
		t.Fatal("invalid record accepted")
	}
}

func TestItemAndSnapshotValidationAreCanonicalAndBounded(t *testing.T) {
	fixture := newOrdinaryResumeFixture(t, 0x51)
	if _, err := NewItem("../escape", ItemIncomplete, ItemBlockNone); err == nil {
		t.Fatal("noncanonical item path accepted")
	}
	if _, err := NewItem("result/file", ItemBlocked, ItemBlockNone); err == nil {
		t.Fatal("blocked item without reason accepted")
	}
	if _, err := NewItem("result/file", ItemResumable, ItemBlockPublicationUnknown); err == nil {
		t.Fatal("ordinary item with blocked reason accepted")
	}
	if _, err := NewBlockedReference(""); err == nil {
		t.Fatal("empty diagnostic reference accepted")
	}
	if _, err := NewBlockedReference(string(bytes.Repeat([]byte{'x'}, MaximumDiagnosticReferenceBytes+1))); err == nil {
		t.Fatal("oversized diagnostic reference accepted")
	}
	reference, _ := NewBlockedReference("checkpoint-01")
	second := resumeItem(t, "result/z", ItemPublished)
	first := resumeItem(t, "result/a", ItemResumable)
	snapshot, err := NewSnapshot(resumeHeader(t, fixture.record), []Item{second, reference, first})
	if err != nil {
		t.Fatal(err)
	}
	items := snapshot.Items()
	if items[0].Reference() != "checkpoint-01" || items[1].CanonicalPath() != "result/a" {
		t.Fatalf("snapshot ordering = %+v", items)
	}
	if _, err := NewSnapshot(resumeHeader(t, fixture.record), []Item{first, first}); err == nil {
		t.Fatal("duplicate artifact path accepted")
	}
}

func TestResumeVocabularyAndPublicValuesPreserveExactAuthority(t *testing.T) {
	for state, want := range map[OperationState]string{
		OperationIncomplete: "incomplete", OperationResumable: "resumable",
		OperationCleanupPending: "cleanup-pending", OperationNeedsAttention: "operation-needs-attention",
		OperationDiscarded: "discarded",
	} {
		if state.String() != want {
			t.Fatalf("operation state %d = %q", state, state.String())
		}
	}
	for state, want := range map[ItemState]string{
		ItemIncomplete: "incomplete", ItemResumable: "resumable", ItemPublished: "published",
		ItemFailed: "failed", ItemBlocked: "item-blocked",
	} {
		if state.String() != want {
			t.Fatalf("item state %d = %q", state, state.String())
		}
	}
	for reason, want := range map[ItemBlockReason]string{
		ItemBlockNone:               "none",
		ItemBlockPublicationUnknown: "publication-unknown",
		ItemBlockCheckpointInvalid:  "checkpoint-invalid",
		ItemBlockOwnedObjectUnknown: "owned-object-unknown",
	} {
		if reason.String() != want {
			t.Fatalf("block reason %d = %q", reason, reason.String())
		}
	}

	fixture := newOrdinaryResumeFixture(t, 0x59)
	blocked, err := NewItem("result/blocked", ItemBlocked, ItemBlockOwnedObjectUnknown)
	if err != nil || blocked.BlockReason() != ItemBlockOwnedObjectUnknown {
		t.Fatalf("blocked item = (%+v, %v)", blocked, err)
	}
	header := resumeHeader(t, fixture.record)
	snapshot, err := NewSnapshot(header, []Item{blocked})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := ReduceRecovery(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if summary.OperationID() != fixture.intent.OperationID() ||
		summary.ReceiveIntentDigest() != fixture.intent.Digest() ||
		summary.StateGeneration() != fixture.record.LifecycleGeneration() ||
		summary.NeedsAttentionReason() != checkpointmodel.OrdinaryReasonNone {
		t.Fatalf("summary authority = %+v", summary)
	}
	cursor := NewPageCursor(summary.OperationID())
	if cursor.IsZero() || cursor.After() != summary.OperationID() {
		t.Fatalf("page cursor = %+v", cursor)
	}
	page, err := NewPage([]Header{header}, cursor, true)
	if err != nil || len(page.Headers()) != 1 || page.Next().After() != summary.OperationID() || !page.Unknown() {
		t.Fatalf("page = (%+v, %v)", page, err)
	}
	if _, err := NewPage([]Header{{}}, PageCursor{}, false); err == nil {
		t.Fatal("page accepted an invalid operation header")
	}
	reference, _ := NewBlockedReference("checkpoint-duplicate")
	if _, err := NewSnapshot(header, []Item{reference, reference}); err == nil {
		t.Fatal("snapshot accepted a duplicate blocked reference")
	}
	if _, err := ReduceRecovery(Snapshot{}); err == nil {
		t.Fatal("recovery accepted an invalid snapshot")
	}
	if _, err := ReduceHeader(Header{}, false); err == nil {
		t.Fatal("header reduction accepted invalid authority")
	}
	if _, err := operationState(checkpointmodel.OrdinaryOperationRecord{}, nil); err == nil {
		t.Fatal("operation reducer accepted an invalid record")
	}
}

func TestInventorySortsOperationsAndSurfacesUnknownRegistryEntries(t *testing.T) {
	first := newOrdinaryResumeFixture(t, 0x61)
	second := newOrdinaryResumeFixture(t, 0x71)
	firstSummary, _ := ReduceHeader(resumeHeader(t, first.record), false)
	secondSummary, _ := ReduceHeader(resumeHeader(t, second.record), true)
	inventory := newInventory([]Summary{secondSummary, firstSummary}, true)
	if inventory.Status() != ListNeedsAttention || !inventory.UnknownEntries() ||
		len(inventory.Summaries()) != 2 || !inventory.Summaries()[1].Busy() {
		t.Fatalf("inventory = %+v", inventory)
	}
	if OperationState(0).String() != "" || ItemState(0).String() != "" ||
		ItemBlockReason(0).String() != "" {
		t.Fatal("invalid vocabulary values have strings")
	}
}
