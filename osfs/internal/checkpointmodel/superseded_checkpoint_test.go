package checkpointmodel

import "testing"

func TestSupersededCheckpointRequiresSameIdentityAndCoveredProgress(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*RecordSpec, *RecordSpec)
		want   bool
	}{
		{name: "several generations later", want: true},
		{name: "same generation with more progress", want: true, change: func(current, candidate *RecordSpec) {
			current.StateGeneration, current.CheckpointGeneration = candidate.StateGeneration, candidate.CheckpointGeneration
		}},
		{name: "uncommitted installed image", change: func(current, _ *RecordSpec) {
			current.CommitState = CommitCandidate
		}},
		{name: "different object", change: func(_, candidate *RecordSpec) {
			candidate.OwnedObjectID = append([]byte(nil), candidate.OwnedObjectID...)
			candidate.OwnedObjectID[0] ^= 1
		}},
		{name: "different revision", change: func(_, candidate *RecordSpec) {
			candidate.FileRevision[0] ^= 1
		}},
		{name: "uncovered saved bytes", change: func(current, _ *RecordSpec) {
			current.VerifiedRanges = []Range{{Offset: 8, End: 64}}
		}},
		{name: "newer checkpoint candidate", change: func(_, candidate *RecordSpec) {
			candidate.CheckpointGeneration = 4
		}},
		{name: "newer lifecycle candidate", change: func(_, candidate *RecordSpec) {
			candidate.StateGeneration = 4
		}},
		{name: "publication receipt", change: func(_, candidate *RecordSpec) {
			candidate.Phase, candidate.CommitState = PhasePublished, CommitPublished
			candidate.VerifiedRanges = []Range{{Offset: 0, End: 64}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			currentSpec := canonicalRecordSpec(t)
			currentSpec.CommitState = CommitVerified
			currentSpec.StateGeneration, currentSpec.CheckpointGeneration = 3, 3
			currentSpec.VerifiedRanges = []Range{{Offset: 0, End: 64}}
			candidateSpec := canonicalRecordSpec(t)
			candidateSpec.CommitState = CommitVerified
			candidateSpec.VerifiedRanges = []Range{{Offset: 0, End: 8}}
			if test.change != nil {
				test.change(&currentSpec, &candidateSpec)
			}
			current := mustCanonicalRecord(t, currentSpec)
			candidate := mustCanonicalRecord(t, candidateSpec)
			if got := SupersedesCheckpoint(current, candidate); got != test.want {
				t.Fatalf("superseded=%t, want %t", got, test.want)
			}
			if SupersedesCheckpoint(Record{}, candidate) || SupersedesCheckpoint(current, Record{}) {
				t.Fatal("invalid record supplied progress evidence")
			}
			if test.name == "several generations later" && ValidateTransition(candidate, current) == nil {
				t.Fatal("supersession weakened the adjacent replacement contract")
			}
		})
	}
}
