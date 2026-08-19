package transfer

import "testing"

func TestItemBlockReasonsProduceExactAuthoritativeOutcomeCounts(t *testing.T) {
	binding, _ := outputLifecycleFixture(t)
	reference, err := NewMaterializationStateRef(binding.OutputSessionID(), binding.Locator().Digest())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		reason ItemBlockReason
		want   FileOutcomeSummary
	}{
		{"revision conflict", ItemBlockRevisionConflict, FileOutcomeSummary{ItemBlockedFiles: 1, RevisionConflictFiles: 1}},
		{"invalid checkpoint", ItemBlockCheckpointInvalid, FileOutcomeSummary{ItemBlockedFiles: 1, CheckpointInvalidFiles: 1}},
		{"owned object conflict", ItemBlockOwnedObjectUnknown, FileOutcomeSummary{ItemBlockedFiles: 1, OwnedObjectUnknownFiles: 1}},
		{"legacy block remains unclassified", ItemBlockOwnershipUnknown, FileOutcomeSummary{ItemBlockedFiles: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settlement, err := NewTransactionItemBlockedFileSettlement(binding, reference, test.reason)
			if err != nil {
				t.Fatal(err)
			}
			tracker := newReceiveProgressTracker()
			tracker.acceptFileSettlement(settlement, binding.ExactSize())
			if got := tracker.snapshotValue().FileOutcomes; got != test.want {
				t.Fatalf("outcomes = %+v, want %+v", got, test.want)
			}
		})
	}

	if ItemBlockReason(0).Valid() || ItemBlockReason(255).Valid() {
		t.Fatal("item-block vocabulary accepted an open-ended enum value")
	}
}
