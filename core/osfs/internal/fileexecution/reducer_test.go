package fileexecution

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
)

type reducerFixture struct {
	engineFixture
	engine *Engine
	object checkpointmodel.ObjectID
	active checkpointmodel.Record
	full   checkpointmodel.Record
}

func newReducerFixture(t *testing.T, exactSize uint64) reducerFixture {
	t.Helper()
	fixture := newEngineFixture(t, exactSize)
	engine := newTestEngine(
		t, fixture,
		&fakeDirectoryAuthority{namespace: newFakePublicNamespace()},
		newFakePlatform(), &fakeCheckpointRepository{},
	)
	key, err := engine.checkpointKey(fixture.claim)
	if err != nil {
		t.Fatal(err)
	}
	object, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{0x41}, transfer.OutputObjectIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := newInitialRecord(key, object)
	if err != nil {
		t.Fatal(err)
	}
	active, err := checkpointmodel.PromoteInitialCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	full := active
	if exactSize != 0 {
		candidate, err = checkpointmodel.AdvanceGeneration(
			active,
			[]checkpointmodel.Range{{Offset: 0, End: exactSize}},
			checkpointmodel.PhaseActive,
			checkpointmodel.CommitCandidate,
		)
		if err != nil {
			t.Fatal(err)
		}
		full, err = checkpointmodel.Promote(
			candidate, checkpointmodel.PhaseActive, checkpointmodel.CommitVerified,
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	return reducerFixture{
		engineFixture: fixture, engine: engine, object: object, active: active, full: full,
	}
}

func mustOwnedObservation(
	t *testing.T,
	object checkpointmodel.ObjectID,
	condition OwnedCondition,
) OwnedObservation {
	t.Helper()
	observation, err := NewOwnedObservation(object, condition)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func mustFinalObservation(t *testing.T, condition FinalCondition) FinalObservation {
	t.Helper()
	observation, err := ObserveFinal(condition)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func TestReduceRecoveryCoversEveryStableLifecycleDecision(t *testing.T) {
	fixture := newReducerFixture(t, 4)
	pausedFull, err := pauseRecord(fixture.full)
	if err != nil {
		t.Fatal(err)
	}
	publishingFull, err := publishingRecord(fixture.full)
	if err != nil {
		t.Fatal(err)
	}
	publishingIncomplete, err := publishingRecord(fixture.active)
	if err != nil {
		t.Fatal(err)
	}
	published, err := publishedRecord(publishingFull)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := retiredRecord(fixture.active, checkpointmodel.RetirementInvalidatedRevision)
	if err != nil {
		t.Fatal(err)
	}
	quarantined, err := quarantineRecord(fixture.active, checkpointmodel.QuarantineAnchorUnsafe)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		record     checkpointmodel.Record
		owned      OwnedCondition
		final      FinalCondition
		action     RecoveryAction
		quarantine checkpointmodel.QuarantineReason
	}{
		{name: "active", record: fixture.active, owned: OwnedReady, final: FinalAbsent, action: RecoveryOpenActive},
		{name: "paused", record: pausedFull, owned: OwnedReady, final: FinalAbsent, action: RecoveryActivate},
		{name: "paused collision", record: pausedFull, owned: OwnedReady, final: FinalCollision, action: RecoveryPublishBlocked},
		{name: "active collision", record: fixture.active, owned: OwnedReady, final: FinalCollision, action: RecoveryInstallQuarantine, quarantine: checkpointmodel.QuarantinePublicationHistory},
		{name: "active unsafe", record: fixture.active, owned: OwnedReady, final: FinalUnsafe, action: RecoveryInstallQuarantine, quarantine: checkpointmodel.QuarantineFinalUnsafe},
		{name: "active unexpected owned final", record: fixture.active, owned: OwnedReady, final: FinalOwnedExact, action: RecoveryInstallQuarantine, quarantine: checkpointmodel.QuarantinePublicationHistory},
		{name: "publishing retry", record: publishingFull, owned: OwnedReady, final: FinalAbsent, action: RecoveryRetryPublication},
		{name: "publishing collision", record: publishingFull, owned: OwnedReady, final: FinalCollision, action: RecoveryPublishBlocked},
		{name: "publishing unsafe", record: publishingFull, owned: OwnedReady, final: FinalUnsafe, action: RecoveryInstallQuarantine, quarantine: checkpointmodel.QuarantineFinalUnsafe},
		{name: "publishing metadata", record: publishingFull, owned: OwnedReady, final: FinalOwnedMetadataMismatch, action: RecoveryInstallQuarantine, quarantine: checkpointmodel.QuarantineMetadataMismatch},
		{name: "publishing complete", record: publishingFull, owned: OwnedReady, final: FinalOwnedExact, action: RecoveryCompletePublication},
		{name: "publishing incomplete", record: publishingIncomplete, owned: OwnedReady, final: FinalAbsent, action: RecoveryNeedsAttention},
		{name: "published", record: published, owned: OwnedStageMissing, final: FinalOwnedExact, action: RecoveryReturnPublished},
		{name: "published mismatch", record: published, owned: OwnedReady, final: FinalCollision, action: RecoveryNeedsAttention},
		{name: "retired", record: retired, owned: OwnedAbsent, final: FinalAbsent, action: RecoveryReturnRetired},
		{name: "retired mismatch", record: retired, owned: OwnedReady, final: FinalCollision, action: RecoveryNeedsAttention},
		{name: "quarantined", record: quarantined, owned: OwnedAbsent, final: FinalAbsent, action: RecoveryReturnQuarantined},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := ReduceRecovery(
				test.record,
				mustOwnedObservation(t, fixture.object, test.owned),
				mustFinalObservation(t, test.final),
			)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Action() != test.action || decision.QuarantineReason() != test.quarantine {
				t.Fatalf("decision=(%v,%v) want (%v,%v)", decision.Action(), decision.QuarantineReason(), test.action, test.quarantine)
			}
		})
	}
}

func TestReduceRecoveryMapsEveryOwnedMismatchWithoutGrantingAHandle(t *testing.T) {
	fixture := newReducerFixture(t, 1)
	tests := []struct {
		condition OwnedCondition
		reason    checkpointmodel.QuarantineReason
	}{
		{OwnedAbsent, checkpointmodel.QuarantineAnchorMissing},
		{OwnedAnchorMissing, checkpointmodel.QuarantineAnchorMissing},
		{OwnedAnchorUnsafe, checkpointmodel.QuarantineAnchorUnsafe},
		{OwnedStageMissing, checkpointmodel.QuarantineStageMissing},
		{OwnedStageMismatch, checkpointmodel.QuarantineStageMismatch},
		{OwnedStageUnsafe, checkpointmodel.QuarantineStageUnsafe},
		{OwnedObjectCollision, checkpointmodel.QuarantineOutputObjectDuplicate},
	}
	for _, test := range tests {
		decision, err := ReduceRecovery(
			fixture.active,
			mustOwnedObservation(t, fixture.object, test.condition),
			mustFinalObservation(t, FinalAbsent),
		)
		if err != nil || decision.Action() != RecoveryInstallQuarantine ||
			decision.QuarantineReason() != test.reason {
			t.Fatalf("condition=%v decision=(%v,%v) err=%v", test.condition, decision.Action(), decision.QuarantineReason(), err)
		}
	}
	if _, err := ReduceRecovery(checkpointmodel.Record{}, OwnedObservation{}, FinalObservation{}); !errors.Is(err, ErrCheckpointBinding) {
		t.Fatalf("invalid record: %v", err)
	}
	if _, err := ReduceRecovery(fixture.active, OwnedObservation{}, mustFinalObservation(t, FinalAbsent)); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("invalid observation: %v", err)
	}
}

func TestCheckpointAndExpectationTypesExposeOnlyComparisonFacts(t *testing.T) {
	fixture := newReducerFixture(t, 2)
	key, err := fixture.engine.checkpointKey(fixture.claim)
	if err != nil {
		t.Fatal(err)
	}
	if key.TransferIntentDigest() != fixture.intent.Digest() ||
		key.FileID() != fixture.file.Descriptor.FileID() ||
		key.FileRevision() != fixture.file.Descriptor.FileRevision() ||
		key.CanonicalPath() != fixture.file.Path || key.ExactSize() != fixture.file.ExpectedSize ||
		key.BackendID() != fixture.intent.BackendID() || key.RootIdentity() != fixture.ownership.RootIdentity() {
		t.Fatalf("checkpoint key lost binding facts: %#v", key)
	}
	identity, err := outputIdentity(fixture.object)
	if err != nil {
		t.Fatal(err)
	}
	expectation, err := NewFinalExpectation(identity, fixture.file.ExpectedSize, fixture.file.Descriptor.ModifiedTime())
	if err != nil {
		t.Fatal(err)
	}
	if expectation.ObjectIdentity() != identity || expectation.ExactSize() != fixture.file.ExpectedSize ||
		expectation.ModifiedTime() != fixture.file.Descriptor.ModifiedTime() {
		t.Fatal("final expectation lost comparison facts")
	}
	if _, err := NewFinalExpectation(transfer.OutputObjectIdentity{}, 0, fixture.file.Descriptor.ModifiedTime()); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("zero expectation: %v", err)
	}
	if _, err := NewOwnedObservation(checkpointmodel.ObjectID{}, OwnedReady); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("zero owned observation: %v", err)
	}
	if _, err := ObserveFinal(0); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("zero final observation: %v", err)
	}
	if _, err := ObservedCheckpoint(checkpointmodel.Record{}); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("invalid checkpoint observation: %v", err)
	}
	if _, present := MissingCheckpoint().Record(); present {
		t.Fatal("missing checkpoint reported a record")
	}
}
