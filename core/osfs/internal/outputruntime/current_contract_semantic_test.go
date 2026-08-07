package outputruntime

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestCurrentRuntimeInventoryMappingsAreExhaustive(t *testing.T) {
	t.Parallel()

	phaseCases := []struct {
		state resumestate.CheckpointRuntimePhase
		want  FilesystemOutputFilePhase
	}{
		{resumestate.CheckpointRuntimeReserved, FilesystemOutputFileReserved},
		{resumestate.CheckpointRuntimeWitnessed, FilesystemOutputFileWitnessed},
		{resumestate.CheckpointRuntimePublishing, FilesystemOutputFilePublishing},
		{resumestate.CheckpointRuntimePublishBlocked, FilesystemOutputFilePublishBlocked},
		{resumestate.CheckpointRuntimePublished, FilesystemOutputFilePublished},
		{resumestate.CheckpointRuntimeRetiring, FilesystemOutputFileRetiring},
		{resumestate.CheckpointRuntimeQuarantined, FilesystemOutputFileQuarantined},
		{0, 0},
	}
	for _, test := range phaseCases {
		if got := filesystemOutputFilePhaseFromState(test.state); got != test.want {
			t.Errorf("phase %d mapped to %d, want %d", test.state, got, test.want)
		}
	}

	actionCases := []struct {
		state resumestate.RecoveryAction
		want  FilesystemOutputRecoveryAction
	}{
		{resumestate.RecoveryRetryObjectCreation, FilesystemOutputRecoveryRetryObjectCreation},
		{resumestate.RecoveryInstallWitness, FilesystemOutputRecoveryInstallWitness},
		{resumestate.RecoveryRequireRevisionBinding, FilesystemOutputRecoveryRequireRevisionBinding},
		{resumestate.RecoveryResumeContent, FilesystemOutputRecoveryResumeContent},
		{resumestate.RecoveryInstallPublishing, FilesystemOutputRecoveryInstallPublishing},
		{resumestate.RecoveryLinkFinalNoReplace, FilesystemOutputRecoveryLinkFinalNoReplace},
		{resumestate.RecoverySyncFinalParent, FilesystemOutputRecoverySyncFinalParent},
		{resumestate.RecoveryInstallPublished, FilesystemOutputRecoveryInstallPublished},
		{resumestate.RecoveryInstallPublishBlocked, FilesystemOutputRecoveryInstallPublishBlocked},
		{resumestate.RecoveryHoldPublishBlocked, FilesystemOutputRecoveryHoldPublishBlocked},
		{resumestate.RecoveryRemovePublishedStageAndSync, FilesystemOutputRecoveryRemovePublishedStageAndSync},
		{resumestate.RecoverySyncPublishedStageParent, FilesystemOutputRecoverySyncPublishedStageParent},
		{resumestate.RecoveryRemoveRetiringStageAndSync, FilesystemOutputRecoveryRemoveRetiringStageAndSync},
		{resumestate.RecoverySyncStageRemoveAnchorAndSync, FilesystemOutputRecoverySyncStageRemoveAnchorAndSync},
		{resumestate.RecoverySyncParentsRemoveRecordAndSync, FilesystemOutputRecoverySyncParentsRemoveRecordAndSync},
		{resumestate.RecoveryInstallRetiring, FilesystemOutputRecoveryInstallRetiring},
		{resumestate.RecoveryInstallQuarantine, FilesystemOutputRecoveryInstallQuarantine},
		{resumestate.RecoveryHoldQuarantine, FilesystemOutputRecoveryHoldQuarantine},
		{resumestate.RecoveryHoldPublishedCleanup, FilesystemOutputRecoveryHoldPublishedCleanup},
		{resumestate.RecoveryHoldRetiringCleanup, FilesystemOutputRecoveryHoldRetiringCleanup},
		{0, 0},
	}
	for _, test := range actionCases {
		if got := filesystemOutputRecoveryActionFromState(test.state); got != test.want {
			t.Errorf("action %d mapped to %d, want %d", test.state, got, test.want)
		}
	}

	certificationCases := []struct {
		state resumestate.CertificationID
		want  FilesystemOutputCertificationID
	}{
		{resumestate.CertificationLinuxExt4ProcessRestart, FilesystemOutputCertificationLinuxExt4ProcessRestart},
		{resumestate.CertificationWindowsNTFSProcessRestart, FilesystemOutputCertificationWindowsNTFSProcessRestart},
		{"unknown", ""},
	}
	for _, test := range certificationCases {
		if got := filesystemOutputCertificationFromState(test.state); got != test.want {
			t.Errorf("certification %q mapped to %q, want %q", test.state, got, test.want)
		}
	}

	digest := filesystemOutputAncestryDigestFromState(resumestate.OutputAncestryBinding{})
	bytes := digest.Bytes()
	if len(bytes) != len(digest) {
		t.Fatalf("ancestry digest bytes = %d, want %d", len(bytes), len(digest))
	}
	bytes[0] = 1
	if digest[0] != 0 {
		t.Fatal("ancestry digest exposed mutable backing storage")
	}
	locator := resumestate.DigestCanonicalLocator("mapped.bin")
	if outputLocatorDigestFromState(locator) != locator.OutputLocatorDigest() {
		t.Fatal("locator digest mapping changed identity")
	}
	object, err := resumestate.OutputObjectIDFromBytes(make([]byte, resumestate.OutputObjectIDBytes))
	if err == nil || !object.IsZero() {
		t.Fatalf("zero object identity = %v, %v", object, err)
	}
}

func TestCurrentRuntimeCheckpointAndQuarantineMappings(t *testing.T) {
	t.Parallel()

	phaseCases := []struct {
		state resumestate.CheckpointRuntimePhase
		want  resumestate.FileCheckpointPhase
	}{
		{resumestate.CheckpointRuntimeReserved, resumestate.FileCheckpointPhaseReserved},
		{resumestate.CheckpointRuntimeWitnessed, resumestate.FileCheckpointPhaseActive},
		{resumestate.CheckpointRuntimePublishing, resumestate.FileCheckpointPhasePublishing},
		{resumestate.CheckpointRuntimePublishBlocked, resumestate.FileCheckpointPhasePaused},
		{resumestate.CheckpointRuntimePublished, resumestate.FileCheckpointPhasePublished},
		{resumestate.CheckpointRuntimeRetiring, resumestate.FileCheckpointPhaseRetired},
		{resumestate.CheckpointRuntimeQuarantined, resumestate.FileCheckpointPhaseQuarantined},
		{0, resumestate.FileCheckpointPhaseQuarantined},
	}
	for _, test := range phaseCases {
		if got := checkpointPhaseForFile(test.state); got != test.want {
			t.Errorf("checkpoint phase %d = %d, want %d", test.state, got, test.want)
		}
	}

	reasonCases := []struct {
		state resumestate.QuarantineReason
		want  transfer.QuarantineReason
	}{
		{resumestate.QuarantinePublicationHistory, transfer.QuarantinePublicationAmbiguous},
		{resumestate.QuarantineFinalMismatch, transfer.QuarantinePublicationAmbiguous},
		{resumestate.QuarantineFinalUnsafe, transfer.QuarantinePublicationAmbiguous},
		{resumestate.QuarantineMetadataMismatch, transfer.QuarantinePublicationAmbiguous},
		{resumestate.QuarantinePartialObjectCreation, transfer.QuarantineRetirementMismatch},
		{resumestate.QuarantineUpdateTemporary, transfer.QuarantineStateCorrupt},
		{resumestate.QuarantineOutputObjectDuplicate, transfer.QuarantineStateCorrupt},
		{resumestate.QuarantineAnchorMissing, transfer.QuarantineOwnershipMismatch},
		{0, transfer.QuarantineOwnershipMismatch},
	}
	for _, test := range reasonCases {
		if got := mapQuarantineReason(test.state); got != test.want {
			t.Errorf("quarantine reason %d = %d, want %d", test.state, got, test.want)
		}
	}

	cause := errors.New("checkpoint unavailable")
	if checkpointOutputFault("load", nil) != nil {
		t.Fatal("nil checkpoint cause produced a fault")
	}
	if err := checkpointOutputFault("load", cause); !errors.Is(err, cause) {
		t.Fatalf("checkpoint fault lost cause: %v", err)
	}
	if !recoveryActionRetainsObservationCleanup(resumestate.RecoveryInstallQuarantine) ||
		!recoveryActionRetainsObservationCleanup(resumestate.RecoveryHoldQuarantine) ||
		recoveryActionRetainsObservationCleanup(resumestate.RecoveryResumeContent) {
		t.Fatal("recovery cleanup ownership mapping changed")
	}
}

func TestCurrentRuntimeFaultClassificationPreservesAuthorityBoundaries(t *testing.T) {
	t.Parallel()

	ordinary := errors.New("storage unavailable")
	if outputAncestryPauseFault("inspect", nil) != nil || outputAncestryOperationFault("inspect", nil) != nil ||
		outputAncestryCleanupFault("inspect", nil) != nil {
		t.Fatal("nil ancestry cause produced a fault")
	}
	for _, err := range []error{
		outputAncestryPauseFault("inspect", ordinary),
		outputAncestryOperationFault("inspect", ordinary),
		outputAncestryOperationFault("inspect", errOutputAncestryUnsafe),
		outputAncestryOperationFault("inspect", outputcap.ErrUnsafeNamespace),
		outputAncestryCleanupFault("inspect", ordinary),
		outputAncestrySessionFault("inspect", ordinary, false),
		outputAncestrySessionFault("inspect", ordinary, true),
	} {
		if err == nil {
			t.Fatal("ancestry failure was discarded")
		}
	}

	decisionCases := []struct {
		err  error
		want FilesystemOutputAncestryDecision
	}{
		{nil, FilesystemOutputAncestryMatched},
		{errOutputAncestryMismatch, FilesystemOutputAncestryMismatch},
		{errOutputAncestryAuthorityDenied, FilesystemOutputAncestryAuthorityDenied},
		{errOutputAncestryUnsafe, FilesystemOutputAncestryStructuralUnsafe},
		{outputcap.ErrUnsafeNamespace, FilesystemOutputAncestryStructuralUnsafe},
		{ordinary, FilesystemOutputAncestryAuthorityDenied},
	}
	for _, test := range decisionCases {
		if got := outputAncestryTraceDecision(test.err); got != test.want {
			t.Errorf("ancestry decision for %v = %d, want %d", test.err, got, test.want)
		}
	}
	if outputAncestryAdmissionBoundary(false) != FilesystemOutputAncestryAdmission ||
		outputAncestryAdmissionBoundary(true) != FilesystemOutputAncestryRestart {
		t.Fatal("ancestry admission boundary mapping changed")
	}
	if classifyOutputAncestryEvidence(nil) != nil ||
		!errors.Is(classifyOutputAncestryEvidence(errOutputAncestryUnsafe), errOutputAncestryUnsafe) ||
		!errors.Is(classifyOutputAncestryEvidence(outputcap.ErrUnsafeNamespace), errOutputAncestryUnsafe) ||
		!errors.Is(classifyOutputAncestryEvidence(ordinary), ordinary) {
		t.Fatal("ancestry evidence classification changed")
	}

	if classifyNativeRecoveryFailure(nil, nativeBeforeEntryEvidence) != 0 ||
		classifyNativeRecoveryFailure(ordinary, nativeBeforeEntryEvidence) != nativeRecoveryPauseRequired ||
		classifyNativeRecoveryFailure(ordinary, nativeExistingEntryUnclassified) != nativeRecoveryAmbiguous ||
		classifyNativeRecoveryFailure(outputnamespace.ErrPositiveEntryEvidence, nativeAuthorizedMutation) != nativeRecoveryAmbiguous ||
		classifyNativeRecoveryFailure(outputcap.ErrUnsafeNamespace, nativeAuthorizedMutation) != nativeRecoveryAmbiguous ||
		classifyNativeRecoveryFailure(outputcap.ErrNamespaceCollision, nativeAuthorizedMutation) != nativeRecoveryAmbiguous {
		t.Fatal("native recovery classification changed")
	}

	for _, err := range []error{
		pauseRequiredFileOutputFault(ordinary),
		pauseRequiredFileOperationFault("publish", ordinary, nil),
		pauseRequiredFileOperationFault("publish", errOutputAncestryUnsafe, nil),
		pauseRequiredFileOperationFault("publish", nil, ordinary),
		pauseRequiredFileOperationFault("publish", ordinary, ordinary),
		internalCleanupNeedsAttentionFault("cleanup"),
	} {
		if err == nil {
			t.Fatal("pause-required failure was discarded")
		}
	}
	if pauseRequiredFileOperationFault("publish", nil, nil) != nil {
		t.Fatal("empty operation failure produced an error")
	}

	regularFault := fileOutputFault("write", ordinary)
	unsafeFault := fileOutputFault("remove", outputcap.ErrUnsafeNamespace)
	assertCurrentFault(t, regularFault, transfer.OutputFaultFile, transfer.OutputFaultStateIO)
	assertCurrentFault(t, unsafeFault, transfer.OutputFaultFile, transfer.OutputFaultOwnership)
	assertCurrentFault(t, directoryOutputFault("sync", ordinary), transfer.OutputFaultSession, transfer.OutputFaultStateIO)
	if fileSettlementFailure(regularFault) != regularFault || sessionSettlementFailure(regularFault) != regularFault {
		t.Fatal("typed output fault was wrapped again")
	}
	assertCurrentFault(t, fileSettlementFailure(ordinary), transfer.OutputFaultFile, transfer.OutputFaultStateIO)
	assertCurrentFault(t, sessionSettlementFailure(ordinary), transfer.OutputFaultSession, transfer.OutputFaultStateIO)
	if scope, code := filesystemOutputTraceFailure(ordinary); scope != 0 || code != 0 {
		t.Fatalf("ordinary trace failure = (%d, %d)", scope, code)
	}
	if scope, code := filesystemOutputTraceFailure(unsafeFault); scope != transfer.OutputFaultFile || code != transfer.OutputFaultOwnership {
		t.Fatalf("typed trace failure = (%d, %d)", scope, code)
	}

	if outputDirectoryOperationFault("mkdir", nil) != nil {
		t.Fatal("nil directory cause produced a fault")
	}
	for _, cause := range []error{
		errOutputAncestryUnsafe,
		outputcap.ErrUnsafeNamespace,
		errors.Join(outputnamespace.ErrPositiveEntryEvidence, outputcap.ErrNamespaceCollision),
		ordinary,
	} {
		if outputDirectoryOperationFault("mkdir", cause) == nil {
			t.Fatalf("directory cause %v was discarded", cause)
		}
	}
}

func TestCurrentRuntimeAncestrySnapshotsFailClosed(t *testing.T) {
	t.Parallel()

	snapshot := outputAncestrySnapshot{entries: []outputAncestryEntry{{path: ""}, {path: "nested"}}}
	if _, found := snapshot.claim("missing"); found {
		t.Fatal("missing ancestry path unexpectedly had a claim")
	}
	if _, found := snapshot.claim("nested"); !found {
		t.Fatal("retained ancestry path lost its claim")
	}
	if !snapshot.matches(snapshot) || snapshot.matches(outputAncestrySnapshot{}) ||
		snapshot.matches(outputAncestrySnapshot{entries: []outputAncestryEntry{{path: "different"}, {path: "nested"}}}) {
		t.Fatal("ancestry snapshot equality changed")
	}

	var validation *outputAncestryValidation
	if _, err := validation.directory(""); !errors.Is(err, errOutputAncestryUnsafe) {
		t.Fatalf("nil validation directory error = %v", err)
	}
	if err := validation.Revalidate(outputAncestryRequirement{}); !errors.Is(err, errOutputAncestryUnsafe) {
		t.Fatalf("nil validation recheck error = %v", err)
	}
	if err := validation.Close(); err != nil || closeOutputAncestryValidation(nil) != nil {
		t.Fatalf("nil ancestry close = %v", err)
	}
	empty := &outputAncestryValidation{}
	if _, err := empty.directory(""); !errors.Is(err, errOutputAncestryUnsafe) {
		t.Fatalf("unguarded validation directory error = %v", err)
	}
	if err := empty.Close(); err != nil {
		t.Fatalf("empty validation close = %v", err)
	}
	if err := closeOutputAncestryDirectories(map[string]outputcap.Directory{"": nil}, []string{""}); err != nil {
		t.Fatalf("nil retained directory close = %v", err)
	}
}

func TestCurrentRuntimeSessionStateAndClaims(t *testing.T) {
	t.Parallel()

	var nilSession *Session
	nilSession.poisonState()
	if err := nilSession.shutdownOwner(); err != nil || nilSession.SessionID() != (transfer.OutputSessionID{}) ||
		nilSession.Capabilities() != (transfer.OutputCapabilities{}) || nilSession.closeHandles() != nil {
		t.Fatalf("nil session boundary failed: %v", err)
	}
	if nilSession.BackendID() != filesystemOutputBackendID {
		t.Fatal("backend identity must not depend on live session state")
	}
	if !errors.Is(nilSession.beginOperation(), transfer.ErrInvalidOutputBinding) {
		t.Fatal("nil session operation did not fail as an invalid binding")
	}

	session := &Session{
		beginning:    make(map[resumestate.LocatorDigest]struct{}),
		active:       make(map[resumestate.LocatorDigest]*FileTransaction),
		objectClaims: make(map[resumestate.OutputObjectID]resumestate.LocatorDigest),
	}
	digest := resumestate.DigestCanonicalLocator("claimed.bin")
	if err := session.claimFileStart(digest); err != nil {
		t.Fatal(err)
	}
	if err := session.claimFileStart(digest); err == nil {
		t.Fatal("duplicate beginning claim succeeded")
	}
	session.releaseFileStart(digest)
	session.active[digest] = nil
	if err := session.claimFileStart(digest); err == nil {
		t.Fatal("active file claim succeeded")
	}
	delete(session.active, digest)
	for index := range maxFilesystemOutputTransactions {
		session.active[resumestate.DigestCanonicalLocator(fmt.Sprintf("limit-%d", index))] = nil
	}
	if err := session.claimFileStart(digest); err == nil {
		t.Fatal("transaction limit did not reject a new file")
	}
	clear(session.active)
	session.closed = true
	if err := session.claimFileStart(digest); err == nil {
		t.Fatal("closed session accepted a file claim")
	}
	session.closed = false

	object, err := resumestate.OutputObjectIDFromBytes([]byte{
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	session.objectClaims[object] = digest
	session.releaseOutputObjectClaim(object, resumestate.DigestCanonicalLocator("other.bin"))
	if _, found := session.objectClaims[object]; !found {
		t.Fatal("foreign object claim was released")
	}
	session.releaseOutputObjectClaim(object, digest)
	if _, found := session.objectClaims[object]; found {
		t.Fatal("matching object claim was retained")
	}

	if err := session.beginOperation(); err != nil {
		t.Fatal(err)
	}
	session.endOperation()
	session.poisonState()
	if !session.operationDisabled() || !session.stateWritesDisabled() || session.beginOperation() == nil {
		t.Fatal("poisoned session remained operational")
	}
	if err := session.shutdownOwner(); err != nil {
		t.Fatal(err)
	}

	activeSession := &Session{
		beginning: make(map[resumestate.LocatorDigest]struct{}),
		active:    make(map[resumestate.LocatorDigest]*FileTransaction),
	}
	// A live transaction's map key is always its durable locator digest. The zero
	// projection used here therefore belongs under the zero digest as well.
	activeSession.active[resumestate.LocatorDigest{}] = &FileTransaction{session: activeSession, lifecycle: FileTransactionOpen}
	if err := activeSession.shutdownOwner(); err != nil {
		t.Fatal(err)
	}
	if len(activeSession.active) != 0 || !activeSession.closed {
		t.Fatal("shutdown retained an active transaction")
	}

	settling := &Session{beginning: map[resumestate.LocatorDigest]struct{}{}, active: map[resumestate.LocatorDigest]*FileTransaction{}}
	if err := settling.beginSettlement(); err != nil {
		t.Fatal(err)
	}
	settling.endSettlement()
	if settling.beginSettlement() == nil {
		t.Fatal("settling session admitted a second settlement")
	}
	if err := (&Session{closed: true}).beginOperation(); err == nil {
		t.Fatal("closed session admitted an operation")
	}
}

func TestCurrentRuntimeSmallStatePredicates(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		want bool
	}{
		{"00", true}, {"af", true}, {"AF", false}, {"g0", false}, {"0", false}, {"000", false},
	} {
		if got := validStateShard(test.name); got != test.want {
			t.Errorf("validStateShard(%q) = %t, want %t", test.name, got, test.want)
		}
	}
	pauseCases := []struct {
		job  transfer.JobPauseReason
		file transfer.FilePauseReason
	}{
		{transfer.JobPauseInterrupted, transfer.FilePauseInterrupted},
		{transfer.JobPauseShutdown, transfer.FilePauseShutdown},
		{transfer.JobPauseTransportFailure, transfer.FilePauseTransportFailure},
		{transfer.JobPauseSessionFailure, transfer.FilePauseSessionFailure},
		{transfer.JobPauseDependencyContract, transfer.FilePauseOutputFailure},
	}
	for _, test := range pauseCases {
		if got := filePauseReasonForJob(test.job); got != test.file {
			t.Errorf("job pause %d = file pause %d, want %d", test.job, got, test.file)
		}
	}

	if !isMissing(fs.ErrNotExist) || isMissing(errors.New("present")) {
		t.Fatal("missing-entry predicate changed")
	}
	if closeOutputFile(nil) != nil || closeOutputDirectory(nil) != nil || closeOutputLock(nil) != nil {
		t.Fatal("nil capability close produced an error")
	}
	if _, err := (stagedData{}).SameFile(anchorWitness{}); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("invalid publication roles error = %v", err)
	}
}

type currentSemanticCloseFile struct {
	outputcap.File
	err error
}

func (file *currentSemanticCloseFile) Close() error { return file.err }

func TestCurrentRuntimePublicationWitnessOwnsBothRoles(t *testing.T) {
	t.Parallel()

	var nilWitness *publicationWitness
	if err := nilWitness.Close(); err != nil {
		t.Fatal(err)
	}
	stageErr := errors.New("close stage")
	anchorErr := errors.New("close anchor")
	witness := &publicationWitness{
		stage:  stagedData{file: &currentSemanticCloseFile{err: stageErr}},
		anchor: anchorWitness{file: &currentSemanticCloseFile{err: anchorErr}},
	}
	if err := witness.Close(); !errors.Is(err, stageErr) || !errors.Is(err, anchorErr) {
		t.Fatalf("publication witness close = %v", err)
	}
	if witness.stage.valid() || witness.anchor.valid() {
		t.Fatal("publication witness retained closed capabilities")
	}
	if err := witness.Close(); err != nil {
		t.Fatalf("second publication witness close = %v", err)
	}
}

func assertCurrentFault(
	t *testing.T,
	err error,
	wantScope transfer.OutputFaultScope,
	wantCode transfer.OutputFaultCode,
) {
	t.Helper()
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) {
		t.Fatalf("error %v has no output fault", err)
	}
	if fault.Scope() != wantScope || fault.Code() != wantCode {
		t.Fatalf("fault = (%d, %d), want (%d, %d)", fault.Scope(), fault.Code(), wantScope, wantCode)
	}
	if !errors.Is(err, fault) && !errors.Is(fault, err) {
		// The wrapper and the fault may occupy different nodes, but one must be
		// reachable through the standard error chain for observability to work.
		t.Fatalf("fault %v is not reachable from %v", fault, err)
	}
}
