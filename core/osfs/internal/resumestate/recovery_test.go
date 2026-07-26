package resumestate

import (
	"errors"
	"reflect"
	"testing"
)

type recoveryCase struct {
	name        string
	phase       FilePhase
	observation FileObservation
	action      RecoveryAction
	settlement  RecoverySettlement
	next        FilePhase
	reason      QuarantineReason
	retirement  RetirementReason
}

func TestRecoveryActionTraceValuesRemainStable(t *testing.T) {
	cases := []struct {
		action RecoveryAction
		want   uint8
	}{
		{RecoveryRetryObjectCreation, 1},
		{RecoveryInstallWitness, 2},
		{RecoveryRequireRevisionBinding, 3},
		{RecoveryResumeContent, 4},
		{RecoveryInstallPublishing, 5},
		{RecoveryLinkFinalNoReplace, 6},
		{RecoverySyncFinalParent, 7},
		{RecoveryInstallPublished, 8},
		{RecoveryInstallPublishBlocked, 9},
		{RecoveryHoldPublishBlocked, 10},
		{RecoveryRemovePublishedStageAndSync, 11},
		{RecoverySyncPublishedStageParent, 12},
		{RecoveryRemoveRetiringStageAndSync, 13},
		{RecoverySyncStageRemoveAnchorAndSync, 14},
		{RecoverySyncParentsRemoveRecordAndSync, 15},
		{RecoveryInstallRetiring, 16},
		{RecoveryInstallQuarantine, 17},
		{RecoveryHoldQuarantine, 18},
		{RecoveryHoldPublishedCleanup, 19},
		{RecoveryHoldRetiringCleanup, 20},
	}
	for _, test := range cases {
		if got := uint8(test.action); got != test.want {
			t.Fatalf("recovery action %d = %d, want %d", test.action, got, test.want)
		}
	}
}

func TestRecoveryMatrix(t *testing.T) {
	missing := FileObservation{Anchor: AnchorMissing, Stage: EntryMissing, Final: EntryMissing}
	witness := FileObservation{Anchor: AnchorVerified, Stage: EntrySameAsAnchor, Final: EntryMissing}
	matchingFinal := FileObservation{
		Anchor: AnchorVerified, Stage: EntrySameAsAnchor, Final: EntrySameAsAnchor, Metadata: MetadataMatches,
	}
	publishingCut := matchingFinal
	publishingCut.FinalParent = FinalParentSynced
	foreignFinal := FileObservation{Anchor: AnchorVerified, Stage: EntrySameAsAnchor, Final: EntryDifferentFromAnchor}
	cases := []recoveryCase{
		{name: "reserved retry", phase: FileReserved, observation: missing, action: RecoveryRetryObjectCreation, settlement: RecoveryContinuing},
		{name: "reserved collision", phase: FileReserved, observation: FileObservation{Anchor: AnchorMissing, Stage: EntryMissing, Final: EntryPresentUnresolved}, action: RecoveryInstallRetiring, settlement: RecoveryContinuing, next: FileRetiring, retirement: RetirementPreObjectCollision},
		{name: "reserved stage only", phase: FileReserved, observation: FileObservation{Anchor: AnchorMissing, Stage: EntryPresentUnresolved, Final: EntryMissing}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantinePartialObjectCreation},
		{name: "reserved anchor only", phase: FileReserved, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryMissing, Final: EntryMissing}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantinePartialObjectCreation},
		{name: "reserved witness", phase: FileReserved, observation: witness, action: RecoveryInstallWitness, settlement: RecoveryContinuing, next: FileWitnessed},
		{name: "reserved foreign final still witnesses", phase: FileReserved, observation: foreignFinal, action: RecoveryInstallWitness, settlement: RecoveryContinuing, next: FileWitnessed},
		{name: "reserved matching final ambiguous", phase: FileReserved, observation: matchingFinal, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantinePublicationHistory},
		{name: "reserved mismatched internal link", phase: FileReserved, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryDifferentFromAnchor, Final: EntryMissing}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineStageMismatch},

		{name: "witnessed resumes", phase: FileWitnessed, observation: witness, action: RecoveryResumeContent, settlement: RecoveryReadyForContent},
		{name: "witnessed foreign final resumes", phase: FileWitnessed, observation: foreignFinal, action: RecoveryResumeContent, settlement: RecoveryReadyForContent},
		{name: "witnessed missing anchor", phase: FileWitnessed, observation: missing, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineAnchorMissing},
		{name: "witnessed missing stage", phase: FileWitnessed, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryMissing, Final: EntryMissing}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineStageMissing},
		{name: "witnessed matching final ambiguous", phase: FileWitnessed, observation: matchingFinal, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantinePublicationHistory},

		{name: "publishing retries link", phase: FilePublishing, observation: witness, action: RecoveryLinkFinalNoReplace, settlement: RecoveryContinuing},
		{name: "publishing syncs deterministic cut first", phase: FilePublishing, observation: matchingFinal, action: RecoverySyncFinalParent, settlement: RecoveryContinuing},
		{name: "publishing adopts synced deterministic cut", phase: FilePublishing, observation: publishingCut, action: RecoveryInstallPublished, settlement: RecoveryContinuing, next: FilePublished},
		{name: "publishing cut permits removed stage", phase: FilePublishing, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryMissing, Final: EntrySameAsAnchor, Metadata: MetadataMatches, FinalParent: FinalParentSynced}, action: RecoveryInstallPublished, settlement: RecoveryContinuing, next: FilePublished},
		{name: "publishing foreign final ambiguous", phase: FilePublishing, observation: foreignFinal, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantinePublicationHistory},
		{name: "publishing missing stage without final", phase: FilePublishing, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryMissing, Final: EntryMissing}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineStageMissing},
		{name: "publishing metadata mismatch after sync", phase: FilePublishing, observation: FileObservation{Anchor: AnchorVerified, Stage: EntrySameAsAnchor, Final: EntrySameAsAnchor, Metadata: MetadataDiffers, FinalParent: FinalParentSynced}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineMetadataMismatch},
		{name: "publishing matching final with mismatched stage", phase: FilePublishing, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryDifferentFromAnchor, Final: EntrySameAsAnchor, Metadata: MetadataMatches, FinalParent: FinalParentSynced}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineStageMismatch},
		{name: "publishing final present after anchor loss", phase: FilePublishing, observation: FileObservation{Anchor: AnchorMissing, Stage: EntryMissing, Final: EntryPresentUnresolved}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineAnchorMissing},

		{name: "blocked remains collision", phase: FilePublishBlocked, observation: foreignFinal, action: RecoveryHoldPublishBlocked, settlement: RecoveryPublishBlocked},
		{name: "blocked retries without retransmission", phase: FilePublishBlocked, observation: witness, action: RecoveryInstallPublishing, settlement: RecoveryContinuing, next: FilePublishing},
		{name: "blocked matching final ambiguous", phase: FilePublishBlocked, observation: matchingFinal, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantinePublicationHistory},
		{name: "blocked missing anchor", phase: FilePublishBlocked, observation: FileObservation{Anchor: AnchorMissing, Stage: EntryMissing, Final: EntryPresentUnresolved}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineAnchorMissing},
		{name: "blocked missing stage", phase: FilePublishBlocked, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryMissing, Final: EntryDifferentFromAnchor}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineStageMissing},
		{name: "blocked mismatched stage", phase: FilePublishBlocked, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryDifferentFromAnchor, Final: EntryDifferentFromAnchor}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineStageMismatch},

		{name: "published removes leftover stage", phase: FilePublished, observation: matchingFinal, action: RecoveryRemovePublishedStageAndSync, settlement: RecoveryPublished},
		{name: "published resyncs absent stage parent", phase: FilePublished, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryMissing, Final: EntrySameAsAnchor, Metadata: MetadataMatches}, action: RecoverySyncPublishedStageParent, settlement: RecoveryPublished},
		{name: "published never resurrects final", phase: FilePublished, observation: witness, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineFinalMismatch},
		{name: "published metadata drift", phase: FilePublished, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryMissing, Final: EntrySameAsAnchor, Metadata: MetadataDiffers}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineMetadataMismatch},
		{name: "published missing anchor leaves final unverified", phase: FilePublished, observation: FileObservation{Anchor: AnchorMissing, Stage: EntryMissing, Final: EntryPresentUnresolved}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineFinalMismatch},
		{name: "published different final", phase: FilePublished, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryMissing, Final: EntryDifferentFromAnchor}, action: RecoveryInstallQuarantine, settlement: RecoveryNeedsAttention, next: FileQuarantined, reason: QuarantineFinalMismatch},
		{name: "published mismatched stage holds cleanup", phase: FilePublished, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryDifferentFromAnchor, Final: EntrySameAsAnchor, Metadata: MetadataMatches}, action: RecoveryHoldPublishedCleanup, settlement: RecoveryNeedsAttention},

		{name: "retiring stage first", phase: FileRetiring, observation: FileObservation{Anchor: AnchorVerified, Stage: EntrySameAsAnchor}, action: RecoveryRemoveRetiringStageAndSync, settlement: RecoveryRetiring},
		{name: "retiring anchor second", phase: FileRetiring, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryMissing}, action: RecoverySyncStageRemoveAnchorAndSync, settlement: RecoveryRetiring},
		{name: "retiring record last", phase: FileRetiring, observation: FileObservation{Anchor: AnchorMissing, Stage: EntryMissing}, action: RecoverySyncParentsRemoveRecordAndSync, settlement: RecoveryRetired},
		{name: "retiring cannot prove stage holds cleanup", phase: FileRetiring, observation: FileObservation{Anchor: AnchorMissing, Stage: EntryPresentUnresolved}, action: RecoveryHoldRetiringCleanup, settlement: RecoveryNeedsAttention},
		{name: "retiring mismatched stage holds cleanup", phase: FileRetiring, observation: FileObservation{Anchor: AnchorVerified, Stage: EntryDifferentFromAnchor}, action: RecoveryHoldRetiringCleanup, settlement: RecoveryNeedsAttention},

		{name: "quarantine holds without observation", phase: FileQuarantined, observation: FileObservation{}, action: RecoveryHoldQuarantine, settlement: RecoveryNeedsAttention},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var decision RecoveryDecision
			var err error
			if test.phase == FileWitnessed {
				decision, err = ReduceResumableFileRecovery(testResumableFile(t, test.phase), test.observation)
			} else {
				decision, err = ReduceFileRecovery(testBoundFileRecord(t, test.phase), test.observation)
			}
			if err != nil {
				t.Fatal(err)
			}
			if decision.Action() != test.action || decision.Settlement() != test.settlement ||
				decision.NextPhase() != test.next || decision.QuarantineReason() != test.reason ||
				decision.RetirementReason() != test.retirement {
				t.Fatalf("decision = %+v", decision)
			}
		})
	}
}

func TestPublishedAndRetiringCleanupAmbiguityPreservesDurableState(t *testing.T) {
	cases := []struct {
		name        string
		phase       FilePhase
		observation FileObservation
		action      RecoveryAction
	}{
		{
			name:  "published unsafe stage",
			phase: FilePublished,
			observation: FileObservation{
				Anchor: AnchorVerified, Stage: EntryUnsafe, Final: EntrySameAsAnchor, Metadata: MetadataMatches,
			},
			action: RecoveryHoldPublishedCleanup,
		},
		{
			name:        "retiring unresolved stage without anchor",
			phase:       FileRetiring,
			observation: FileObservation{Anchor: AnchorMissing, Stage: EntryPresentUnresolved},
			action:      RecoveryHoldRetiringCleanup,
		},
		{
			name:        "retiring unsafe stage",
			phase:       FileRetiring,
			observation: FileObservation{Anchor: AnchorVerified, Stage: EntryUnsafe},
			action:      RecoveryHoldRetiringCleanup,
		},
		{
			name:        "retiring unsafe anchor",
			phase:       FileRetiring,
			observation: FileObservation{Anchor: AnchorUnsafe, Stage: EntryMissing},
			action:      RecoveryHoldRetiringCleanup,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			record := testBoundFileRecord(t, test.phase)
			decision, err := ReduceFileRecovery(record, test.observation)
			if err != nil || decision.Action() != test.action ||
				decision.Settlement() != RecoveryNeedsAttention || decision.NextPhase() != 0 ||
				decision.QuarantineReason() != 0 {
				t.Fatalf("decision = %+v, %v", decision, err)
			}
			applied, err := ApplyRecoveryDecision(record, decision)
			if err != nil || !reflect.DeepEqual(applied.Record(), record.Record()) {
				t.Fatalf("held record = %+v, %v", applied.Record(), err)
			}
		})
	}
}

func TestPublishedAndRetiringPartialCleanupEvidenceCannotAuthorizeQuarantine(t *testing.T) {
	cases := []struct {
		phase       FilePhase
		observation FileObservation
	}{
		{
			phase: FilePublished,
			observation: FileObservation{
				Anchor: AnchorVerified, Stage: EntryDifferentFromAnchor, Final: EntryNotObserved,
			},
		},
		{
			phase:       FileRetiring,
			observation: FileObservation{Anchor: AnchorMissing, Stage: EntryPresentUnresolved},
		},
	}
	for _, test := range cases {
		if InternalFileObservationRequiresQuarantine(test.phase, test.observation) {
			t.Fatalf("cleanup evidence authorized quarantine for %v: %+v", test.phase, test.observation)
		}
	}
}

func TestUnsafeAndMismatchedObservationsQuarantineAtFileScope(t *testing.T) {
	cases := []struct {
		name        string
		observation FileObservation
		reason      QuarantineReason
	}{
		{name: "anchor", observation: FileObservation{Anchor: AnchorUnsafe, Stage: EntryMissing, Final: EntryMissing}, reason: QuarantineAnchorUnsafe},
		{name: "stage", observation: FileObservation{Anchor: AnchorVerified, Stage: EntryUnsafe, Final: EntryMissing}, reason: QuarantineStageUnsafe},
		{name: "final", observation: FileObservation{Anchor: AnchorVerified, Stage: EntrySameAsAnchor, Final: EntryUnsafe}, reason: QuarantineFinalUnsafe},
		{name: "stage mismatch", observation: FileObservation{Anchor: AnchorVerified, Stage: EntryDifferentFromAnchor, Final: EntryMissing}, reason: QuarantineStageMismatch},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			decision, err := ReduceFileRecovery(testBoundFileRecord(t, FileWitnessed), test.observation)
			if err != nil || decision.NextPhase() != FileQuarantined || decision.QuarantineReason() != test.reason {
				t.Fatalf("decision = %+v, %v", decision, err)
			}
		})
	}
}

func TestDirectPublishResultIsDistinctFromRestartObservation(t *testing.T) {
	record := testBoundFileRecord(t, FilePublishing)
	direct, err := ReducePublishResult(record, PublishAlreadyExistsDifferent)
	if err != nil || direct.Action() != RecoveryInstallPublishBlocked || direct.NextPhase() != FilePublishBlocked ||
		direct.Settlement() != RecoveryPublishBlocked {
		t.Fatalf("direct collision = %+v, %v", direct, err)
	}
	restarted, err := ReduceFileRecovery(record, FileObservation{
		Anchor: AnchorVerified, Stage: EntrySameAsAnchor, Final: EntryDifferentFromAnchor,
	})
	if err != nil || restarted.NextPhase() != FileQuarantined {
		t.Fatalf("restart collision = %+v, %v", restarted, err)
	}
	linked, err := ReducePublishResult(record, PublishLinkCreated)
	if err != nil || linked.Action() != RecoverySyncFinalParent || linked.NextPhase() != 0 {
		t.Fatalf("linked result = %+v, %v", linked, err)
	}
	if _, err := ReducePublishResult(record, 0); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid publish result error = %v", err)
	}
	if _, err := ReducePublishResult(testBoundFileRecord(t, FileWitnessed), PublishLinkCreated); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid publish phase error = %v", err)
	}
}

func TestRecoveryDecisionApplicationAdvancesStateOnlyWhenRequired(t *testing.T) {
	record := testBoundFileRecord(t, FileReserved)
	decision, err := ReduceFileRecovery(record, FileObservation{
		Anchor: AnchorVerified, Stage: EntrySameAsAnchor, Final: EntryMissing,
	})
	if err != nil {
		t.Fatal(err)
	}
	next, err := ApplyRecoveryDecision(record, decision)
	if err != nil || next.Record().Phase() != FileWitnessed ||
		next.Record().StateGeneration() != record.Record().StateGeneration()+1 ||
		next.Record().CheckpointGeneration() != record.Record().CheckpointGeneration() {
		t.Fatalf("applied = %+v, %v", next, err)
	}
	holdDecision, err := ReduceFileRecovery(next, FileObservation{
		Anchor: AnchorVerified, Stage: EntrySameAsAnchor, Final: EntryMissing,
	})
	if err != nil {
		t.Fatal(err)
	}
	hold, err := ApplyRecoveryDecision(next, holdDecision)
	if err != nil || !hold.valid() || hold.Record().StateGeneration() != next.Record().StateGeneration() {
		t.Fatalf("held = %+v, %v", hold, err)
	}
	if _, err := ApplyRecoveryDecision(record, RecoveryDecision{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero decision error = %v", err)
	}
	if _, err := ApplyRecoveryDecision(next, decision); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("replayed decision error = %v", err)
	}
	differentRecord := record
	differentRecord.record.outputObject = identity32[OutputObjectID](0xee)
	if !differentRecord.valid() {
		t.Fatal("cross-record decision fixture is invalid")
	}
	if _, err := ApplyRecoveryDecision(differentRecord, decision); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("cross-record decision error = %v", err)
	}
}

func TestPreObjectCollisionSettlesOnlyAfterCrashRecoverableRecordRemoval(t *testing.T) {
	reserved := testBoundFileRecord(t, FileReserved)
	installRetiring, err := ReduceFileRecovery(reserved, FileObservation{
		Anchor: AnchorMissing, Stage: EntryMissing, Final: EntryPresentUnresolved,
	})
	if err != nil || installRetiring.Settlement() != RecoveryContinuing {
		t.Fatalf("initial collision cut = %+v, %v", installRetiring, err)
	}
	retiring, err := ApplyRecoveryDecision(reserved, installRetiring)
	if err != nil || retiring.Record().RetirementReason() != RetirementPreObjectCollision {
		t.Fatalf("retiring collision = %+v, %v", retiring, err)
	}

	finish, err := ReduceFileRecovery(retiring, FileObservation{Anchor: AnchorMissing, Stage: EntryMissing})
	if err != nil || finish.Action() != RecoverySyncParentsRemoveRecordAndSync ||
		finish.Settlement() != RecoveryCollision {
		t.Fatalf("restarted collision cleanup = %+v, %v", finish, err)
	}
	if _, err := ApplyRecoveryDecision(retiring, finish); err != nil {
		t.Fatalf("finish collision cleanup: %v", err)
	}

	matchingCut, err := ReduceFileRecovery(retiring, FileObservation{
		Anchor: AnchorVerified, Stage: EntrySameAsAnchor,
	})
	if err != nil || matchingCut.Action() != RecoveryRemoveRetiringStageAndSync ||
		matchingCut.Settlement() != RecoveryRetiring {
		t.Fatalf("reason-independent retiring cut = %+v, %v", matchingCut, err)
	}
}

func TestReducerIsExhaustiveForEveryNormalizedObservation(t *testing.T) {
	for phase := FileReserved; phase <= FileQuarantined; phase++ {
		record := testBoundFileRecord(t, phase)
		for anchor := AnchorMissing; anchor <= AnchorUnsafe; anchor++ {
			for stage := EntryNotObserved; stage <= EntryUnsafe; stage++ {
				for final := EntryNotObserved; final <= EntryUnsafe; final++ {
					for metadata := MetadataNotObserved; metadata <= MetadataUnsafe; metadata++ {
						for finalParent := FinalParentNotObserved; finalParent <= FinalParentSynced; finalParent++ {
							observation := FileObservation{
								Anchor: anchor, Stage: stage, Final: final, Metadata: metadata, FinalParent: finalParent,
							}
							decision, err := ReduceFileRecovery(record, observation)
							if phase == FileQuarantined {
								if err != nil || decision.Action() == 0 {
									t.Fatalf("quarantined observation %+v = %+v, %v", observation, decision, err)
								}
								continue
							}
							validationErr := validateObservation(phase, observation)
							internalQuarantine := final == EntryNotObserved &&
								InternalFileObservationRequiresQuarantine(phase, observation)
							if validationErr == nil && (err != nil || decision.Action() == 0 || decision.Settlement() == 0) {
								t.Fatalf("valid %v observation %+v = %+v, %v", phase, observation, decision, err)
							}
							if validationErr != nil && !internalQuarantine && !errors.Is(err, ErrInvalidState) {
								t.Fatalf("invalid %v observation %+v error = %v", phase, observation, err)
							}
							if (validationErr == nil || internalQuarantine) && err == nil {
								assertRecoverySafetyProperties(t, phase, observation, decision)
								if _, applyErr := ApplyRecoveryDecision(record, decision); applyErr != nil {
									t.Fatalf("valid %v observation %+v produced unappliable decision %+v: %v", phase, observation, decision, applyErr)
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestWitnessedResumeRequiresIndependentRevisionAuthority(t *testing.T) {
	observation := FileObservation{Anchor: AnchorVerified, Stage: EntrySameAsAnchor, Final: EntryMissing}
	stored, err := ReduceFileRecovery(testBoundFileRecord(t, FileWitnessed), observation)
	if err != nil || stored.Action() != RecoveryRequireRevisionBinding || stored.Settlement() != RecoveryContinuing {
		t.Fatalf("stored witness decision = %+v, %v", stored, err)
	}
	resumable, err := ReduceResumableFileRecovery(testResumableFile(t, FileWitnessed), observation)
	if err != nil || resumable.Action() != RecoveryResumeContent || resumable.Settlement() != RecoveryReadyForContent {
		t.Fatalf("resumable witness decision = %+v, %v", resumable, err)
	}
}

func assertRecoverySafetyProperties(t *testing.T, phase FilePhase, observation FileObservation, decision RecoveryDecision) {
	t.Helper()
	switch decision.Action() {
	case RecoveryInstallPublished:
		if phase != FilePublishing || observation.Anchor != AnchorVerified ||
			(observation.Stage != EntryMissing && observation.Stage != EntrySameAsAnchor) ||
			observation.Final != EntrySameAsAnchor || observation.FinalParent != FinalParentSynced ||
			observation.Metadata != MetadataMatches {
			t.Fatalf("unsafe publication decision for %v %+v", phase, observation)
		}
	case RecoveryRemoveRetiringStageAndSync:
		if phase != FileRetiring || observation.Anchor != AnchorVerified || observation.Stage != EntrySameAsAnchor {
			t.Fatalf("unsafe stage removal decision for %v %+v", phase, observation)
		}
	case RecoverySyncStageRemoveAnchorAndSync:
		if phase != FileRetiring || observation.Anchor != AnchorVerified || observation.Stage != EntryMissing {
			t.Fatalf("unsafe anchor removal decision for %v %+v", phase, observation)
		}
	case RecoveryResumeContent:
		t.Fatalf("stored-only exhaustive reducer released content for %v %+v", phase, observation)
	}
}
