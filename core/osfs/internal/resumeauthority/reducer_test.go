package resumeauthority

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
)

func TestReduceDiscardIsDeterministicAndOrdersEveryDurableCut(t *testing.T) {
	binding, first := resumeRecord(t, 1, 21, checkpointmodel.PhasePaused, checkpointmodel.CommitVerified)
	_, second := resumeRecord(t, 2, 22, checkpointmodel.PhasePaused, checkpointmodel.CommitVerified)
	firstObservation := mustCheckpointObservation(
		t, first, EvidenceExact, EvidenceExact, EvidenceExact,
	)
	secondObservation := mustCheckpointObservation(
		t, second, EvidenceExact, EvidenceAbsent, EvidenceExact,
	)
	firstPublication := mustPublicationObservation(t, first.RecordID(), EvidenceAbsent)
	secondPublication := mustPublicationObservation(t, second.RecordID(), EvidenceAbsent)

	forward := mustSnapshot(t, binding, []CheckpointObservation{firstObservation, secondObservation}, nil)
	reverse := mustSnapshot(t, binding, []CheckpointObservation{secondObservation, firstObservation}, nil)
	forwardPlan := ReduceDiscard(forward, []PublicationObservation{firstPublication, secondPublication})
	reversePlan := ReduceDiscard(reverse, []PublicationObservation{secondPublication, firstPublication})
	if !forwardPlan.Valid() || forwardPlan.ExpectedStatus() != Discarded ||
		len(forwardPlan.Attention()) != 0 {
		t.Fatalf("forward plan = %+v", forwardPlan)
	}
	if !reflect.DeepEqual(actionSignature(forwardPlan), actionSignature(reversePlan)) {
		t.Fatalf("permuted plan changed: %v != %v", actionSignature(forwardPlan), actionSignature(reversePlan))
	}

	records := []checkpointmodel.Record{first, second}
	slices.SortFunc(records, func(left, right checkpointmodel.Record) int {
		return bytes.Compare(left.RecordID().Bytes(), right.RecordID().Bytes())
	})
	expected := make([]string, 0, 11)
	for _, record := range records {
		if record.RecordID() == first.RecordID() {
			expected = append(expected, actionKey(record, ActionRemoveStage))
		}
		expected = append(expected, actionKey(record, ActionSyncStages))
		expected = append(expected,
			actionKey(record, ActionRemoveAnchor),
			actionKey(record, ActionSyncAnchors),
			actionKey(record, ActionRemoveRecord),
			actionKey(record, ActionSyncRecords),
		)
	}
	if actual := actionSignature(forwardPlan); !slices.Equal(actual, expected) {
		t.Fatalf("actions = %v, want %v", actual, expected)
	}
}

func TestReduceDiscardRequiresExactPublishedFinalAndNeverTargetsIt(t *testing.T) {
	binding, record := resumeRecord(t, 3, 23, checkpointmodel.PhasePublished, checkpointmodel.CommitPublished)
	checkpoint := mustCheckpointObservation(t, record, EvidenceExact, EvidenceExact, EvidenceExact)
	snapshot := mustSnapshot(t, binding, []CheckpointObservation{checkpoint}, nil)
	plan := ReduceDiscard(snapshot, []PublicationObservation{
		mustPublicationObservation(t, record.RecordID(), EvidenceExact),
	})
	if !plan.Valid() || plan.ExpectedStatus() != Discarded {
		t.Fatalf("published plan = %+v", plan)
	}
	for _, action := range plan.Actions() {
		if action.Kind() < ActionRemoveStage || action.Kind() > ActionSyncRecords {
			t.Fatalf("unexpected action targets public output: %+v", action)
		}
	}

	for name, evidence := range map[string]Evidence{
		"missing":   EvidenceAbsent,
		"replaced":  EvidenceReplaced,
		"ambiguous": EvidenceAmbiguous,
	} {
		t.Run(name, func(t *testing.T) {
			attentionPlan := ReduceDiscard(snapshot, []PublicationObservation{
				mustPublicationObservation(t, record.RecordID(), evidence),
			})
			if !attentionPlan.Valid() || attentionPlan.ExpectedStatus() != DiscardNeedsAttention ||
				len(attentionPlan.Actions()) != 0 {
				t.Fatalf("unsafe published plan = %+v", attentionPlan)
			}
			wantReason := AttentionAmbiguousPublication
			if evidence == EvidenceReplaced {
				wantReason = AttentionReplacement
			}
			if attentionPlan.Attention()[0].Reason() != wantReason {
				t.Fatalf("reason = %s, want %s", attentionPlan.Attention()[0].Reason(), wantReason)
			}
		})
	}
}

func TestReduceDiscardRecognizesTerminalPublishedHistory(t *testing.T) {
	binding, retired := resumeRecordWithClaims(
		t, 31, 41, checkpointmodel.PhaseRetired, checkpointmodel.CommitVerified,
		0, 0, checkpointmodel.RetirementPublished,
	)
	_, quarantined := resumeRecordWithClaims(
		t, 32, 42, checkpointmodel.PhaseQuarantined, checkpointmodel.CommitQuarantined,
		checkpointmodel.QuarantineMetadataMismatch,
		checkpointmodel.QuarantineOriginPublished,
		0,
	)
	for _, record := range []checkpointmodel.Record{retired, quarantined} {
		checkpoint := mustCheckpointObservation(t, record, EvidenceExact, EvidenceExact, EvidenceExact)
		plan := ReduceDiscard(
			mustSnapshot(t, binding, []CheckpointObservation{checkpoint}, nil),
			[]PublicationObservation{mustPublicationObservation(t, record.RecordID(), EvidenceExact)},
		)
		if !plan.Valid() || plan.ExpectedStatus() != Discarded {
			t.Fatalf("published-history plan = %+v", plan)
		}
	}
}

func TestReduceDiscardNeedsAttentionBoundariesAuthorizeNoMutation(t *testing.T) {
	binding, verified := resumeRecord(t, 4, 24, checkpointmodel.PhasePaused, checkpointmodel.CommitVerified)
	_, candidate := resumeRecord(t, 5, 25, checkpointmodel.PhasePaused, checkpointmodel.CommitCandidate)
	_, published := resumeRecord(t, 6, 26, checkpointmodel.PhasePublished, checkpointmodel.CommitPublished)
	unknown := derivedAttention(AttentionUnknownChildren, []byte("opaque"))

	tests := []struct {
		name         string
		snapshot     RepositorySnapshot
		publications []PublicationObservation
		reason       AttentionReason
	}{
		{
			name:     "missing namespace ownership",
			snapshot: mustSnapshotWithEvidence(t, EvidenceAbsent, checkpointmodel.Binding{}, nil, nil),
			reason:   AttentionMissingOwnership,
		},
		{
			name:     "replaced namespace pin",
			snapshot: mustSnapshotWithEvidence(t, EvidenceReplaced, checkpointmodel.Binding{}, nil, nil),
			reason:   AttentionReplacement,
		},
		{
			name: "replaced record",
			snapshot: mustSnapshot(t, binding, []CheckpointObservation{
				mustAbsentRecordObservation(t, verified.RecordID(), EvidenceReplaced, EvidenceAbsent, EvidenceAbsent),
			}, nil),
			reason: AttentionReplacement,
		},
		{
			name: "uncommitted canonical record",
			snapshot: mustSnapshot(t, binding, []CheckpointObservation{
				mustCheckpointObservation(t, candidate, EvidenceExact, EvidenceExact, EvidenceExact),
			}, nil),
			publications: []PublicationObservation{mustPublicationObservation(t, candidate.RecordID(), EvidenceAbsent)},
			reason:       AttentionCorruptBinding,
		},
		{
			name: "stage ownership ambiguous",
			snapshot: mustSnapshot(t, binding, []CheckpointObservation{
				mustCheckpointObservation(t, verified, EvidenceExact, EvidenceAmbiguous, EvidenceExact),
			}, nil),
			publications: []PublicationObservation{mustPublicationObservation(t, verified.RecordID(), EvidenceAbsent)},
			reason:       AttentionMissingOwnership,
		},
		{
			name: "unknown repository child",
			snapshot: mustSnapshot(t, binding, []CheckpointObservation{
				mustCheckpointObservation(t, verified, EvidenceExact, EvidenceExact, EvidenceExact),
			}, []Attention{unknown}),
			publications: []PublicationObservation{mustPublicationObservation(t, verified.RecordID(), EvidenceAbsent)},
			reason:       AttentionUnknownChildren,
		},
		{
			name: "published final ambiguous",
			snapshot: mustSnapshot(t, binding, []CheckpointObservation{
				mustCheckpointObservation(t, published, EvidenceExact, EvidenceExact, EvidenceExact),
			}, nil),
			publications: []PublicationObservation{mustPublicationObservation(t, published.RecordID(), EvidenceAmbiguous)},
			reason:       AttentionAmbiguousPublication,
		},
		{
			name: "non-published final exists",
			snapshot: mustSnapshot(t, binding, []CheckpointObservation{
				mustCheckpointObservation(t, verified, EvidenceExact, EvidenceExact, EvidenceExact),
			}, nil),
			publications: []PublicationObservation{mustPublicationObservation(t, verified.RecordID(), EvidenceExact)},
			reason:       AttentionAmbiguousPublication,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := ReduceDiscard(test.snapshot, test.publications)
			if !plan.Valid() || plan.ExpectedStatus() != DiscardNeedsAttention || len(plan.Actions()) != 0 {
				t.Fatalf("plan = %+v", plan)
			}
			if !hasAttentionReason(plan.Attention(), test.reason) {
				t.Fatalf("attention = %+v, want %s", plan.Attention(), test.reason)
			}
		})
	}
}

func TestReduceDiscardDistinguishesAlreadyAbsentAndOrphanedArtifacts(t *testing.T) {
	binding, record := resumeRecord(t, 7, 27, checkpointmodel.PhasePaused, checkpointmodel.CommitVerified)
	absent := mustAbsentRecordObservation(t, record.RecordID(), EvidenceAbsent, EvidenceAbsent, EvidenceAbsent)
	plan := ReduceDiscard(mustSnapshot(t, binding, []CheckpointObservation{absent}, nil), nil)
	if !plan.Valid() || plan.ExpectedStatus() != AlreadyAbsent || len(plan.Actions()) != 0 {
		t.Fatalf("absent plan = %+v", plan)
	}

	orphan := mustAbsentRecordObservation(t, record.RecordID(), EvidenceAbsent, EvidenceExact, EvidenceAbsent)
	plan = ReduceDiscard(mustSnapshot(t, binding, []CheckpointObservation{orphan}, nil), nil)
	if !plan.Valid() || plan.ExpectedStatus() != DiscardNeedsAttention ||
		!hasAttentionReason(plan.Attention(), AttentionMissingOwnership) {
		t.Fatalf("orphan plan = %+v", plan)
	}
}

func TestReduceDiscardRejectsDuplicateObjectOwnershipAndMalformedObservations(t *testing.T) {
	binding, first := resumeRecord(t, 8, 28, checkpointmodel.PhasePaused, checkpointmodel.CommitVerified)
	_, second := resumeRecord(t, 9, 28, checkpointmodel.PhasePaused, checkpointmodel.CommitVerified)
	observations := []CheckpointObservation{
		mustCheckpointObservation(t, first, EvidenceExact, EvidenceExact, EvidenceExact),
		mustCheckpointObservation(t, second, EvidenceExact, EvidenceExact, EvidenceExact),
	}
	publications := []PublicationObservation{
		mustPublicationObservation(t, first.RecordID(), EvidenceAbsent),
		mustPublicationObservation(t, second.RecordID(), EvidenceAbsent),
	}
	plan := ReduceDiscard(mustSnapshot(t, binding, observations, nil), publications)
	if !plan.Valid() || plan.ExpectedStatus() != DiscardNeedsAttention ||
		len(plan.Actions()) != 0 || len(plan.Attention()) != 2 {
		t.Fatalf("duplicate ownership plan = %+v", plan)
	}

	malformed := RepositorySnapshot{
		namespaceEvidence: EvidenceExact,
		binding:           binding,
		checkpoints: []CheckpointObservation{{
			recordID: first.RecordID(), record: first, recordEvidence: Evidence(99),
			stageEvidence: EvidenceExact, anchorEvidence: EvidenceExact,
		}},
	}
	plan = ReduceDiscard(malformed, publications[:1])
	if !plan.Valid() || plan.ExpectedStatus() != DiscardNeedsAttention || len(plan.Actions()) != 0 {
		t.Fatalf("malformed observation plan = %+v", plan)
	}
}

func resumeRecord(
	t *testing.T,
	fileSeed byte,
	objectSeed byte,
	phase checkpointmodel.Phase,
	commit checkpointmodel.CommitState,
) (checkpointmodel.Binding, checkpointmodel.Record) {
	return resumeRecordWithClaims(t, fileSeed, objectSeed, phase, commit, 0, 0, 0)
}

func resumeRecordWithClaims(
	t *testing.T,
	fileSeed byte,
	objectSeed byte,
	phase checkpointmodel.Phase,
	commit checkpointmodel.CommitState,
	quarantineReason checkpointmodel.QuarantineReason,
	quarantineOrigin checkpointmodel.QuarantineOrigin,
	retirementReason checkpointmodel.RetirementReason,
) (checkpointmodel.Binding, checkpointmodel.Record) {
	t.Helper()
	intent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0x31}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	backend, err := transfer.NewOutputBackendID("resumeauthority-test")
	if err != nil {
		t.Fatal(err)
	}
	root := bytes.Repeat([]byte{0x41}, sha256.Size)
	ownership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Backend:             backend,
		Certification:       checkpointmodel.CertificationLinuxExt4ProcessRestart,
		RootIdentity:        root,
		RootOpenDisposition: checkpointmodel.CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := checkpointmodel.NewBinding(ownership, intent)
	if err != nil {
		t.Fatal(err)
	}
	var fileID catalog.FileID
	var revision content.FileRevision
	for index := range fileID {
		fileID[index] = fileSeed + byte(index)
		revision[index] = fileSeed + byte(index) + 1
	}
	record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		TransferIntentDigest: intent,
		FileID:               fileID,
		FileRevision:         revision,
		CanonicalPath:        fmt.Sprintf("folder/file-%02x.bin", fileSeed),
		ExactSize:            64,
		BackendID:            string(backend),
		RootIdentity:         root,
		OwnedOutputObject:    bytes.Repeat([]byte{objectSeed}, sha256.Size),
		StateGeneration:      2,
		CheckpointGeneration: 1,
		VerifiedRanges:       []checkpointmodel.Range{{Offset: 0, End: 16}},
		Phase:                phase,
		CommitState:          commit,
		QuarantineReason:     quarantineReason,
		QuarantineOrigin:     quarantineOrigin,
		RetirementReason:     retirementReason,
	})
	if err != nil {
		t.Fatal(err)
	}
	return binding, record
}

func mustCheckpointObservation(
	t *testing.T,
	record checkpointmodel.Record,
	recordEvidence Evidence,
	stage Evidence,
	anchor Evidence,
) CheckpointObservation {
	t.Helper()
	observation, err := NewCheckpointObservation(record.RecordID(), record, recordEvidence, stage, anchor)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func mustAbsentRecordObservation(
	t *testing.T,
	recordID checkpointmodel.RecordID,
	recordEvidence Evidence,
	stage Evidence,
	anchor Evidence,
) CheckpointObservation {
	t.Helper()
	observation, err := NewCheckpointObservation(
		recordID, checkpointmodel.Record{}, recordEvidence, stage, anchor,
	)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func mustPublicationObservation(
	t *testing.T,
	recordID checkpointmodel.RecordID,
	evidence Evidence,
) PublicationObservation {
	t.Helper()
	observation, err := NewPublicationObservation(recordID, evidence)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func mustSnapshot(
	t *testing.T,
	binding checkpointmodel.Binding,
	checkpoints []CheckpointObservation,
	attention []Attention,
) RepositorySnapshot {
	t.Helper()
	return mustSnapshotWithEvidence(t, EvidenceExact, binding, checkpoints, attention)
}

func mustSnapshotWithEvidence(
	t *testing.T,
	evidence Evidence,
	binding checkpointmodel.Binding,
	checkpoints []CheckpointObservation,
	attention []Attention,
) RepositorySnapshot {
	t.Helper()
	snapshot, err := NewRepositorySnapshot(evidence, binding, checkpoints, attention)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func actionSignature(plan DiscardPlan) []string {
	actions := plan.Actions()
	result := make([]string, len(actions))
	for index, action := range actions {
		result[index] = actionKey(action.Record(), action.Kind())
	}
	return result
}

func actionKey(record checkpointmodel.Record, kind ActionKind) string {
	return fmt.Sprintf("%x/%d", record.RecordID().Bytes(), kind)
}

func hasAttentionReason(attention []Attention, reason AttentionReason) bool {
	return slices.ContainsFunc(attention, func(current Attention) bool {
		return current.Reason() == reason
	})
}
