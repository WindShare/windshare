package resumestate

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestNamespaceNamesAreCanonicalShardedAndRoundTrip(t *testing.T) {
	namespace := identity32[transfer.TransferIntentDigest](0xab)
	session := identity16[transfer.OutputSessionID](0xcd)
	object := identity32[OutputObjectID](0xef)
	digest := identity32[LocatorDigest](0x12)
	if got, want := SessionDirectorySegments(namespace, session), []string{
		SessionsDirectoryName, IntentNamespaceName(namespace), strings.Repeat("cd", transfer.OutputSessionIdentityBytes),
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("session segments = %v, want %v", got, want)
	}
	if got := SessionDirectoryName(session); got != strings.Repeat("cd", transfer.OutputSessionIdentityBytes) {
		t.Fatalf("session directory name = %q", got)
	}
	candidateName := SessionCandidateName(session)
	parsedCandidate, candidateErr := ParseSessionCandidateName(candidateName)
	if candidateErr != nil || parsedCandidate != session {
		t.Fatalf("session candidate round trip = %v, %v", parsedCandidate, candidateErr)
	}
	fileName := FileRecordName(digest)
	anchorName := AnchorName(object)
	stageName := StageName(object)
	if fileName.Shard() != "12" || anchorName.Shard() != "ef" || stageName.Shard() != "ef" {
		t.Fatalf("shards = %q %q %q", fileName.Shard(), anchorName.Shard(), stageName.Shard())
	}
	if got := FileRecordSegments(digest); !reflect.DeepEqual(got, []string{FilesDirectoryName, "12", digest.String() + ".state"}) {
		t.Fatalf("file segments = %v", got)
	}
	if got := AnchorSegments(object); !reflect.DeepEqual(got, []string{AnchorsDirectoryName, "ef", object.String() + ".anchor"}) {
		t.Fatalf("anchor segments = %v", got)
	}
	if got := StageSegments(object); !reflect.DeepEqual(got, []string{StagesDirectoryName, "ef", object.String() + ".stage"}) {
		t.Fatalf("stage segments = %v", got)
	}
	parsedNamespace, namespaceErr := ParseIntentNamespaceName(IntentNamespaceName(namespace))
	parsedSession, sessionErr := ParseSessionDirectoryName(strings.Repeat("cd", transfer.OutputSessionIdentityBytes))
	parsedFile, fileErr := ParseFileRecordName(fileName.Shard(), fileName.Name())
	parsedAnchor, anchorErr := ParseAnchorName(anchorName.Shard(), anchorName.Name())
	parsedStage, stageErr := ParseStageName(stageName.Shard(), stageName.Name())
	if namespaceErr != nil || sessionErr != nil || fileErr != nil || anchorErr != nil || stageErr != nil ||
		parsedNamespace != namespace || parsedSession != session || parsedFile != digest || parsedAnchor != object || parsedStage != object {
		t.Fatalf("round trip = %v %v %v %v %v; errors = %v %v %v %v %v",
			parsedNamespace, parsedSession, parsedFile, parsedAnchor, parsedStage,
			namespaceErr, sessionErr, fileErr, anchorErr, stageErr)
	}
}

func TestNamespaceParsingRejectsAliasesMisShardingAndTraversal(t *testing.T) {
	object := identity32[OutputObjectID](0xab)
	valid := AnchorName(object)
	tests := []struct {
		name  string
		shard string
		entry string
	}{
		{name: "wrong shard", shard: "cd", entry: valid.Name()},
		{name: "uppercase", shard: "AB", entry: strings.ToUpper(valid.Name())},
		{name: "suffix", shard: valid.Shard(), entry: object.String() + ".stage"},
		{name: "traversal", shard: "..", entry: valid.Name()},
		{name: "short", shard: "a", entry: valid.Name()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseAnchorName(test.shard, test.entry); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("parse error = %v", err)
			}
		})
	}
	intent := identity32[transfer.TransferIntentDigest](0xab)
	canonicalName := IntentNamespaceName(intent)
	aliasName := strings.ToUpper(canonicalName)
	if _, err := ParseIntentNamespaceName(aliasName); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("uppercase namespace error = %v", err)
	}
	canonical := ClassifyIntentNamespaceName(canonicalName)
	alias := ClassifyIntentNamespaceName(aliasName)
	opaque := ClassifyIntentNamespaceName("not-a-resume-intent")
	if canonical.Classification() != IntentNamespaceCanonical || canonical.Intent() != intent ||
		alias.Classification() != IntentNamespaceDecodableAlias || alias.Intent() != intent ||
		opaque.Classification() != IntentNamespaceOpaque || !opaque.Intent().IsZero() {
		t.Fatalf("namespace classifications = %+v %+v %+v", canonical, alias, opaque)
	}
	if _, err := ParseSessionDirectoryName(strings.Repeat("00", transfer.OutputSessionIdentityBytes)); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero session error = %v", err)
	}
	for _, invalid := range []string{
		SessionCandidatePrefix,
		SessionCandidatePrefix + strings.Repeat("00", transfer.OutputSessionIdentityBytes),
		SessionCandidatePrefix + strings.ToUpper(SessionDirectoryName(identity16[transfer.OutputSessionID](0xab))),
		SessionCandidatePrefix + SessionDirectoryName(identity16[transfer.OutputSessionID](0xab)) + "/child",
		SessionDirectoryName(identity16[transfer.OutputSessionID](0xab)),
	} {
		if _, err := ParseSessionCandidateName(invalid); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("session candidate %q error = %v", invalid, err)
		}
	}
}

func TestBootstrapCandidateNamesBindCleanupAuthority(t *testing.T) {
	nonce := identity32[BootstrapNonce](0xab)
	name := BootstrapCandidateName(nonce)
	parsed, err := ParseBootstrapCandidateName(name)
	if err != nil || parsed != nonce {
		t.Fatalf("candidate parse = %v, %v", parsed, err)
	}
	for _, invalid := range []string{
		ControlDirectoryName, BootstrapCandidatePrefix, BootstrapCandidatePrefix + strings.ToUpper(nonce.String()),
		BootstrapCandidatePrefix + nonce.String() + "/child",
	} {
		if _, err := ParseBootstrapCandidateName(invalid); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("candidate %q error = %v", invalid, err)
		}
	}
}

func TestCorruptionClassificationRequiresEnoughNamespaceAuthorityForIsolation(t *testing.T) {
	for _, kind := range []RecordKind{
		RecordControlDirectoryBinding, RecordGlobalControl, RecordCoordinatorLock, RecordSessionsDirectory,
	} {
		classification, err := ClassifyGlobalCorruption(kind)
		if err != nil || classification.Disposition() != CorruptionBlockOutputRoot ||
			!classification.Intent().IsZero() || !classification.Locator().IsZero() {
			t.Fatalf("global corruption %d = %+v, %v", kind, classification, err)
		}
	}
	if _, err := ClassifyGlobalCorruption(0); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid global kind error = %v", err)
	}

	intent := identity32[transfer.TransferIntentDigest](0xab)
	canonicalIntent := IntentNamespaceName(intent)
	for _, name := range []string{canonicalIntent, strings.ToUpper(canonicalIntent)} {
		classification := ClassifyHeaderCorruption(name)
		if classification.Disposition() != CorruptionBlockResumeNamespace ||
			classification.Intent() != intent || !classification.Locator().IsZero() {
			t.Fatalf("header corruption %q = %+v", name, classification)
		}
	}
	opaqueHeader := ClassifyHeaderCorruption("not-an-intent")
	if opaqueHeader.Disposition() != CorruptionRetainOpaque || !opaqueHeader.Intent().IsZero() {
		t.Fatalf("opaque header corruption = %+v", opaqueHeader)
	}

	namespace := testSessionAuthority(t, SessionActive).NamespaceAuthority()
	digest := DigestCanonicalLocator("folder/file.bin")
	canonicalFile := FileRecordName(digest)
	fileCases := []struct {
		shard       string
		name        string
		disposition CorruptionDisposition
		locator     LocatorDigest
	}{
		{shard: canonicalFile.Shard(), name: canonicalFile.Name(), disposition: CorruptionQuarantineFile, locator: digest},
		{shard: "ff", name: canonicalFile.Name(), disposition: CorruptionQuarantineFile, locator: digest},
		{shard: "zz", name: "opaque", disposition: CorruptionRetainOpaque},
	}
	for _, test := range fileCases {
		entry := ClassifyFileShardEntry(test.shard, test.name)
		classification, err := ClassifyFileCorruption(namespace, entry)
		if err != nil || classification.Disposition() != test.disposition ||
			classification.Intent() != namespace.Header().IntentDigest() || classification.Locator() != test.locator {
			t.Fatalf("file corruption %q/%q = %+v, %v", test.shard, test.name, classification, err)
		}
	}
	if _, err := ClassifyFileCorruption(SessionNamespaceAuthority{}, ClassifyFileShardEntry(canonicalFile.Shard(), canonicalFile.Name())); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unbound file corruption error = %v", err)
	}
	temporaryName := UpdateTemporaryName(digest, identity32[UpdateNonce](0xab))
	temporary := ClassifyFileShardEntry(temporaryName.Shard(), temporaryName.Name())
	if temporary.Classification() != FileShardEntryUpdateTemporary || temporary.Locator() != digest ||
		temporary.Nonce() != identity32[UpdateNonce](0xab) {
		t.Fatalf("unified temporary classification = %+v", temporary)
	}
	if _, err := ClassifyFileCorruption(namespace, temporary); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("canonical temporary was also accepted as file corruption: %v", err)
	}
	decision, err := ReduceUpdateTemporary(namespace, temporary, UpdateTemporaryEntryRegular, UpdateTargetValid)
	if err != nil || decision.Action() != UpdateTemporaryRemoveAndSyncShard {
		t.Fatalf("canonical temporary recovery = %+v, %v", decision, err)
	}
}

func TestBootstrapReducerExhaustivelyHandlesCandidateCuts(t *testing.T) {
	control, err := NewControl(ControlSpec{
		Backend: testBackend(t), OutputRoot: testRootBindingFor(t, CertificationWindowsNTFSProcessRestart, 1),
		Certification: "windows/ntfs/process-restart/v1", Durability: transfer.DurabilityProcessRestart,
		Generation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := NewBootstrapIntent(control, identity32[BootstrapNonce](2))
	if err != nil || intent.Control() != control || intent.Nonce().IsZero() || intent.CandidateName() == "" {
		t.Fatalf("intent = %+v, %v", intent, err)
	}
	for installed := InstalledControlMissing; installed <= InstalledControlUnsafe; installed++ {
		for candidate := BootstrapCandidateMissing; candidate <= BootstrapCandidateUnsafe; candidate++ {
			parent := BootstrapParentNotObserved
			if installed == InstalledControlMatches {
				parent = BootstrapParentSyncRequired
			}
			decision, err := ReduceBootstrap(intent, BootstrapObservation{
				Installed: installed, InstalledParent: parent, Candidate: candidate,
			})
			if err != nil || decision.Action == 0 || decision.Settlement == 0 {
				t.Fatalf("bootstrap %d/%d = %+v, %v", installed, candidate, decision, err)
			}
			unsafe := installed == InstalledControlDiffers || installed == InstalledControlUnsafe ||
				candidate == BootstrapCandidateUnsafe
			if unsafe && (decision.Action != BootstrapBlockOutputRoot || decision.Settlement != BootstrapNeedsAttention) {
				t.Fatalf("unsafe bootstrap %d/%d = %+v", installed, candidate, decision)
			}
		}
	}
	tests := []struct {
		observation BootstrapObservation
		action      BootstrapAction
		settlement  BootstrapSettlement
	}{
		{BootstrapObservation{Installed: InstalledControlMissing, Candidate: BootstrapCandidateMissing}, BootstrapCreateCandidate, BootstrapContinuing},
		{BootstrapObservation{Installed: InstalledControlMissing, Candidate: BootstrapCandidateEmpty}, BootstrapRemoveOwnedCandidate, BootstrapContinuing},
		{BootstrapObservation{Installed: InstalledControlMissing, Candidate: BootstrapCandidateValidPartial}, BootstrapContinueCandidate, BootstrapContinuing},
		{BootstrapObservation{Installed: InstalledControlMissing, Candidate: BootstrapCandidateComplete}, BootstrapInstallCandidateNoReplace, BootstrapContinuing},
		{BootstrapObservation{Installed: InstalledControlMatches, InstalledParent: BootstrapParentSyncRequired, Candidate: BootstrapCandidateMissing}, BootstrapSyncOutputRoot, BootstrapContinuing},
		{BootstrapObservation{Installed: InstalledControlMatches, InstalledParent: BootstrapParentSynced, Candidate: BootstrapCandidateComplete}, BootstrapRemoveOwnedCandidate, BootstrapContinuing},
		{BootstrapObservation{Installed: InstalledControlMatches, InstalledParent: BootstrapParentSynced, Candidate: BootstrapCandidateMissing}, BootstrapUseInstalledControl, BootstrapReady},
	}
	for _, test := range tests {
		decision, err := ReduceBootstrap(intent, test.observation)
		if err != nil || decision.Action != test.action || decision.Settlement != test.settlement {
			t.Fatalf("bootstrap %+v = %+v, %v", test.observation, decision, err)
		}
	}
	if _, err := ReduceBootstrap(BootstrapIntent{}, BootstrapObservation{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid bootstrap error = %v", err)
	}
}
