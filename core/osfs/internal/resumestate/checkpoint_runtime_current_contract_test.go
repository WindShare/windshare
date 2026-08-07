package resumestate

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

func TestCheckpointRuntimeProjectionOwnsTheV1Lifecycle(t *testing.T) {
	binding, descriptor := currentCheckpointRuntimeFixture(t)
	object := identity32[OutputObjectID](0x41)
	runtimeFile, err := NewCheckpointRuntimeFile(binding, descriptor, "runtime.bin", object)
	if err != nil {
		t.Fatal(err)
	}
	reserved := runtimeFile.BoundState()
	if reserved.State().Phase() != CheckpointRuntimeReserved ||
		runtimeFile.Bound().State().LocatorDigest() != reserved.State().LocatorDigest() {
		t.Fatalf("new runtime state = %+v", reserved.State())
	}

	observation := FileObservation{
		Anchor: AnchorVerified, Stage: EntrySameAsAnchor, Final: EntryMissing,
	}
	installWitness, err := ReduceCheckpointRuntimeStateRecovery(reserved, observation)
	if err != nil || installWitness.Action() != RecoveryInstallWitness {
		t.Fatalf("reserved recovery = %+v, %v", installWitness, err)
	}
	witnessed, err := ApplyCheckpointRuntimeRecoveryDecision(reserved, installWitness)
	if err != nil || witnessed.State().Phase() != CheckpointRuntimeWitnessed {
		t.Fatalf("installed witness = %+v, %v", witnessed.State(), err)
	}
	witnessedFile, err := BindCheckpointRuntimeDescriptor(witnessed, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	withoutRevision, err := ReduceCheckpointRuntimeStateRecovery(witnessed, observation)
	if err != nil || withoutRevision.Action() != RecoveryRequireRevisionBinding {
		t.Fatalf("unbound witnessed recovery = %+v, %v", withoutRevision, err)
	}
	withRevision, err := ReduceCheckpointRuntimeFileRecovery(witnessedFile, observation)
	if err != nil || withRevision.Action() != RecoveryResumeContent {
		t.Fatalf("revision-bound witnessed recovery = %+v, %v", withRevision, err)
	}

	complete, err := witnessedFile.WithCheckpoint(1, testRanges(t, content.Range{Offset: 0, End: 10}))
	if err != nil {
		t.Fatal(err)
	}
	publishing, err := PrepareCheckpointRuntimePublication(complete)
	if err != nil || publishing.State().Phase() != CheckpointRuntimePublishing {
		t.Fatalf("publishing state = %+v, %v", publishing.State(), err)
	}
	blockedDecision, err := ReduceCheckpointRuntimePublishResult(publishing, PublishAlreadyExistsDifferent)
	if err != nil || blockedDecision.Action() != RecoveryInstallPublishBlocked {
		t.Fatalf("publish collision decision = %+v, %v", blockedDecision, err)
	}
	blocked, err := ApplyCheckpointRuntimeRecoveryDecision(publishing, blockedDecision)
	if err != nil || blocked.State().Phase() != CheckpointRuntimePublishBlocked {
		t.Fatalf("publish-blocked state = %+v, %v", blocked.State(), err)
	}
	if _, err := ReduceCheckpointRuntimePublishResult(publishing, PublishLinkCreated); err != nil {
		t.Fatal(err)
	}
	if _, err := ReduceCheckpointRuntimePublishResult(publishing, PublishExistingAmbiguous); err != nil {
		t.Fatal(err)
	}

	isolated, err := PrepareCheckpointRuntimeIsolatedRetirement(witnessed)
	if err != nil || isolated.State().RetirementReason() != RetirementIsolatedFailure {
		t.Fatalf("isolated retirement = %+v, %v", isolated.State(), err)
	}
	invalidated, err := PrepareCheckpointRuntimeInvalidatedRevisionRetirement(witnessed)
	if err != nil || invalidated.State().RetirementReason() != RetirementInvalidatedRevision {
		t.Fatalf("revision retirement = %+v, %v", invalidated.State(), err)
	}
	quarantined, err := PrepareCheckpointRuntimeUnsafeNamespaceQuarantine(witnessed, QuarantineStageUnsafe)
	if err != nil || quarantined.State().QuarantineReason() != QuarantineStageUnsafe ||
		quarantined.State().PhaseBeforeQuarantine() != FileWitnessed {
		t.Fatalf("unsafe quarantine = %+v, %v", quarantined.State(), err)
	}

	if _, err := NewCheckpointRuntimeFile(CheckpointRuntimeBinding{}, descriptor, "runtime.bin", object); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero runtime binding = %v", err)
	}
	if _, err := BindCheckpointRuntimeFile(binding, FileRecord{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero runtime record = %v", err)
	}
	foreign := reserved.State()
	foreign.sessionID = identity16[transfer.OutputSessionID](0x42)
	if binding.validRecord(foreign) {
		t.Fatal("runtime binding accepted a foreign session record")
	}
	if _, err := PrepareCheckpointRuntimePublication(CheckpointRuntimeFile{}); err == nil {
		t.Fatalf("zero publication authority = %v", err)
	}
	if _, err := PrepareCheckpointRuntimeIsolatedRetirement(BoundCheckpointRuntimeState{}); err == nil {
		t.Fatalf("zero isolated retirement = %v", err)
	}
	if _, err := PrepareCheckpointRuntimeInvalidatedRevisionRetirement(BoundCheckpointRuntimeState{}); err == nil {
		t.Fatalf("zero revision retirement = %v", err)
	}
	if _, err := PrepareCheckpointRuntimeUnsafeNamespaceQuarantine(BoundCheckpointRuntimeState{}, QuarantineStageUnsafe); err == nil {
		t.Fatalf("zero quarantine authority = %v", err)
	}
}

func TestRestoreCheckpointRuntimeFilePreservesTerminalSemantics(t *testing.T) {
	binding, descriptor := currentCheckpointRuntimeFixture(t)
	for _, test := range []struct {
		name       string
		phase      FileCheckpointPhase
		commit     FileCheckpointCommitState
		want       CheckpointRuntimePhase
		quarantine QuarantineReason
		origin     FilePhase
		retirement RetirementReason
	}{
		{name: "active", phase: FileCheckpointPhaseActive, commit: FileCheckpointCommitVerified, want: CheckpointRuntimeWitnessed},
		{name: "publishing", phase: FileCheckpointPhasePublishing, commit: FileCheckpointCommitVerified, want: CheckpointRuntimePublishing},
		{name: "publish-blocked", phase: FileCheckpointPhasePaused, commit: FileCheckpointCommitVerified, want: CheckpointRuntimePublishBlocked},
		{name: "published", phase: FileCheckpointPhasePublished, commit: FileCheckpointCommitPublished, want: CheckpointRuntimePublished},
		{name: "retiring", phase: FileCheckpointPhaseRetired, commit: FileCheckpointCommitVerified, want: CheckpointRuntimeRetiring, retirement: RetirementIsolatedFailure},
		{name: "quarantined", phase: FileCheckpointPhaseQuarantined, commit: FileCheckpointCommitQuarantined, want: CheckpointRuntimeQuarantined, quarantine: QuarantineStageUnsafe, origin: FileWitnessed},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkpoint := currentRuntimeCheckpoint(t, binding, descriptor, test.phase, test.commit,
				test.quarantine, test.origin, test.retirement)
			restored, err := RestoreCheckpointRuntimeFile(binding, descriptor, checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			state := restored.BoundState().State()
			if state.Phase() != test.want || state.QuarantineReason() != test.quarantine ||
				state.PhaseBeforeQuarantine() != test.origin || state.RetirementReason() != test.retirement ||
				state.StateGeneration() != checkpoint.StateGeneration() ||
				state.CheckpointGeneration() != checkpoint.CheckpointGeneration() {
				t.Fatalf("restored state = %+v", state)
			}
		})
	}

	active := currentRuntimeCheckpoint(
		t, binding, descriptor, FileCheckpointPhaseActive, FileCheckpointCommitVerified, 0, 0, 0,
	)
	if _, err := RestoreCheckpointRuntimeFile(CheckpointRuntimeBinding{}, descriptor, active); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero restore binding = %v", err)
	}
	foreign := binding
	foreign.IntentDigest = identity32[transfer.TransferIntentDigest](0x43)
	if _, err := RestoreCheckpointRuntimeFile(foreign, descriptor, active); !errors.Is(err, ErrFileCheckpointBinding) {
		t.Fatalf("foreign restore binding = %v", err)
	}
	wrongDescriptor := testDescriptor(t, testSessionAuthority(t, SessionActive), 9)
	if _, err := RestoreCheckpointRuntimeFile(binding, wrongDescriptor, active); !errors.Is(err, ErrFileCheckpointBinding) {
		t.Fatalf("foreign restore descriptor = %v", err)
	}
	corrupt := active
	corrupt.checksum[0] ^= 0xff
	if _, err := RestoreCheckpointRuntimeFile(binding, descriptor, corrupt); !errors.Is(err, ErrFileCheckpointChecksum) {
		t.Fatalf("corrupt restore checkpoint = %v", err)
	}
}

func TestCheckpointLifecycleClaimsAndTransitionsAreOrthogonal(t *testing.T) {
	for _, test := range []struct {
		name       string
		phase      FileCheckpointPhase
		quarantine QuarantineReason
		origin     FilePhase
		retirement RetirementReason
		want       bool
	}{
		{name: "ordinary", phase: FileCheckpointPhaseActive, want: true},
		{name: "quarantine", phase: FileCheckpointPhaseQuarantined, quarantine: QuarantineStageUnsafe, origin: FileWitnessed, want: true},
		{name: "retirement", phase: FileCheckpointPhaseRetired, retirement: RetirementIsolatedFailure, want: true},
		{name: "quarantine missing reason", phase: FileCheckpointPhaseQuarantined, origin: FileWitnessed},
		{name: "quarantine missing origin", phase: FileCheckpointPhaseQuarantined, quarantine: QuarantineStageUnsafe},
		{name: "quarantine from quarantine", phase: FileCheckpointPhaseQuarantined, quarantine: QuarantineStageUnsafe, origin: FileQuarantined},
		{name: "quarantine with retirement", phase: FileCheckpointPhaseQuarantined, quarantine: QuarantineStageUnsafe, origin: FileWitnessed, retirement: RetirementIsolatedFailure},
		{name: "retirement missing reason", phase: FileCheckpointPhaseRetired},
		{name: "retirement with quarantine", phase: FileCheckpointPhaseRetired, quarantine: QuarantineStageUnsafe, origin: FileWitnessed, retirement: RetirementIsolatedFailure},
		{name: "ordinary with quarantine", phase: FileCheckpointPhaseActive, quarantine: QuarantineStageUnsafe, origin: FileWitnessed},
		{name: "ordinary with retirement", phase: FileCheckpointPhaseActive, retirement: RetirementIsolatedFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateCheckpointLifecycleClaims(test.phase, test.quarantine, test.origin, test.retirement)
			if (err == nil) != test.want {
				t.Fatalf("lifecycle claims error = %v, want valid:%t", err, test.want)
			}
		})
	}

	base := FileCheckpointV1{phase: FileCheckpointPhaseActive, commitState: FileCheckpointCommitVerified}
	for _, test := range []struct {
		name     string
		previous FileCheckpointPhase
		next     FileCheckpointPhase
		commit   FileCheckpointCommitState
		want     bool
	}{
		{name: "reserved-active", previous: FileCheckpointPhaseReserved, next: FileCheckpointPhaseActive, commit: FileCheckpointCommitVerified, want: true},
		{name: "reserved-retired", previous: FileCheckpointPhaseReserved, next: FileCheckpointPhaseRetired, commit: FileCheckpointCommitVerified, want: true},
		{name: "active-paused", previous: FileCheckpointPhaseActive, next: FileCheckpointPhasePaused, commit: FileCheckpointCommitVerified, want: true},
		{name: "active-publishing", previous: FileCheckpointPhaseActive, next: FileCheckpointPhasePublishing, commit: FileCheckpointCommitVerified, want: true},
		{name: "paused-active", previous: FileCheckpointPhasePaused, next: FileCheckpointPhaseActive, commit: FileCheckpointCommitVerified, want: true},
		{name: "publishing-published", previous: FileCheckpointPhasePublishing, next: FileCheckpointPhasePublished, commit: FileCheckpointCommitPublished, want: true},
		{name: "publishing-quarantine", previous: FileCheckpointPhasePublishing, next: FileCheckpointPhaseQuarantined, commit: FileCheckpointCommitQuarantined, want: true},
		{name: "published-wrong-commit", previous: FileCheckpointPhasePublishing, next: FileCheckpointPhasePublished, commit: FileCheckpointCommitVerified},
		{name: "quarantine-wrong-commit", previous: FileCheckpointPhaseActive, next: FileCheckpointPhaseQuarantined, commit: FileCheckpointCommitVerified},
		{name: "ordinary-wrong-commit", previous: FileCheckpointPhaseActive, next: FileCheckpointPhasePaused, commit: FileCheckpointCommitPublished},
		{name: "commit-regression", previous: FileCheckpointPhasePublishing, next: FileCheckpointPhasePaused, commit: FileCheckpointCommitCandidate},
		{name: "terminal-source", previous: FileCheckpointPhasePublished, next: FileCheckpointPhaseRetired, commit: FileCheckpointCommitVerified},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous, next := base, base
			previous.phase = test.previous
			next.phase, next.commitState = test.next, test.commit
			if got := validCheckpointLifecycleTransition(previous, next); got != test.want {
				t.Fatalf("transition %d -> %d/%d = %t, want %t", test.previous, test.next, test.commit, got, test.want)
			}
		})
	}

	binding, descriptor := currentCheckpointRuntimeFixture(t)
	active := currentRuntimeCheckpoint(
		t, binding, descriptor, FileCheckpointPhaseActive, FileCheckpointCommitVerified, 0, 0, 0,
	)
	publishing, err := AdvanceCheckpointState(
		active, active.StateGeneration()+1, FileCheckpointPhasePublishing, FileCheckpointCommitVerified, 0, 0, 0,
	)
	if err != nil || publishing.CheckpointGeneration() != active.CheckpointGeneration() {
		t.Fatalf("lifecycle-only checkpoint advance = %+v, %v", publishing, err)
	}
	quarantined, err := AdvanceCheckpointState(
		publishing, publishing.StateGeneration()+1, FileCheckpointPhaseQuarantined,
		FileCheckpointCommitQuarantined, QuarantinePublicationHistory, FilePublishing, 0,
	)
	if err != nil || quarantined.QuarantineReason() != QuarantinePublicationHistory ||
		quarantined.PhaseBeforeQuarantine() != FilePublishing {
		t.Fatalf("quarantine checkpoint advance = %+v, %v", quarantined, err)
	}
	if _, err := AdvanceCheckpointState(
		active, active.StateGeneration(), FileCheckpointPhasePublishing, FileCheckpointCommitVerified, 0, 0, 0,
	); !errors.Is(err, ErrFileCheckpointGeneration) {
		t.Fatalf("non-monotonic lifecycle advance = %v", err)
	}
}

func currentCheckpointRuntimeFixture(t *testing.T) (CheckpointRuntimeBinding, content.FileRevisionDescriptor) {
	t.Helper()
	selection := testSelection(t, 10)
	session := testSessionAuthorityForSelection(t, selection, SessionActive)
	binding, err := NewCheckpointRuntimeBinding(
		session.Header().SessionID(), selectionIntentDigest(selection), transfer.NativeFilesystemOutputBackendID,
		bytes.Repeat([]byte{0x31}, sha256.Size),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !binding.valid() {
		t.Fatal("new checkpoint runtime binding is invalid")
	}
	if _, err := NewCheckpointRuntimeBinding(
		transfer.OutputSessionID{}, binding.IntentDigest, binding.BackendID, binding.RootIdentity.Bytes(),
	); !errors.Is(err, ErrFileCheckpointBinding) {
		t.Fatalf("zero checkpoint session ID = %v", err)
	}
	if _, err := NewCheckpointRuntimeBinding(
		binding.SessionID, binding.IntentDigest, "", binding.RootIdentity.Bytes(),
	); !errors.Is(err, ErrFileCheckpointBinding) {
		t.Fatalf("empty checkpoint backend = %v", err)
	}
	return binding, testDescriptor(t, session, 10)
}

func currentRuntimeCheckpoint(
	t *testing.T,
	binding CheckpointRuntimeBinding,
	descriptor content.FileRevisionDescriptor,
	phase FileCheckpointPhase,
	commit FileCheckpointCommitState,
	quarantine QuarantineReason,
	origin FilePhase,
	retirement RetirementReason,
) FileCheckpointV1 {
	t.Helper()
	checkpoint, err := NewFileCheckpointV1(FileCheckpointSpec{
		TransferIntentDigest:  binding.IntentDigest,
		FileID:                descriptor.FileID(),
		FileRevision:          descriptor.FileRevision(),
		CanonicalPath:         "runtime.bin",
		ExactSize:             descriptor.ExactSize(),
		BackendID:             string(binding.BackendID),
		RootIdentity:          binding.RootIdentity.Bytes(),
		OwnedOutputObject:     identity32[OutputObjectID](0x41).Bytes(),
		StateGeneration:       7,
		CheckpointGeneration:  1,
		VerifiedRanges:        []FileCheckpointRange{{Offset: 0, End: descriptor.ExactSize()}},
		Phase:                 phase,
		CommitState:           commit,
		QuarantineReason:      quarantine,
		PhaseBeforeQuarantine: origin,
		RetirementReason:      retirement,
	})
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint
}
