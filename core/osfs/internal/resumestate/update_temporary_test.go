package resumestate

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestUpdateTemporaryNamesBindTargetAndIndependentNonce(t *testing.T) {
	target := identity32[LocatorDigest](0x12)
	nonce := identity32[UpdateNonce](0xab)
	name := UpdateTemporaryName(target, nonce)
	classified := ClassifyFileShardEntry(name.Shard(), name.Name())
	if name.Shard() != "12" || classified.Classification() != FileShardEntryUpdateTemporary ||
		classified.Locator() != target || classified.Nonce() != nonce {
		t.Fatalf("canonical temporary = %+v %+v", name, classified)
	}
	generated, err := GenerateUpdateNonce(bytes.NewReader(bytes.Repeat([]byte{0xcd}, UpdateNonceBytes)))
	if err != nil || generated != identity32[UpdateNonce](0xcd) {
		t.Fatalf("generated nonce = %v, %v", generated, err)
	}
	for _, reader := range []interface{ Read([]byte) (int, error) }{
		nil, bytes.NewReader(nil), bytes.NewReader(make([]byte, UpdateNonceBytes)),
	} {
		if _, err := GenerateUpdateNonce(reader); err == nil {
			t.Fatal("invalid update nonce source accepted")
		}
	}
}

func TestRecordUpdateTemporaryGrammarHasOneCanonicalSpelling(t *testing.T) {
	nonce := identity32[UpdateNonce](0xab)
	target := identity32[LocatorDigest](0x12)
	fileTarget := FileRecordName(target)
	fileName, err := RecordUpdateTemporaryName(fileTarget.Name(), nonce)
	if err != nil || fileName != UpdateTemporaryName(target, nonce).Name() {
		t.Fatalf("file temporary = %q, %v", fileName, err)
	}
	headerName, err := RecordUpdateTemporaryName(HeaderRecordName, nonce)
	if err != nil || headerName != HeaderUpdateTemporaryPrefix+nonce.String() {
		t.Fatalf("header temporary = %q, %v", headerName, err)
	}
	parsed, err := ParseRecordUpdateTemporaryName(HeaderRecordName, headerName)
	if err != nil || parsed != nonce {
		t.Fatalf("parsed header nonce = %v, %v", parsed, err)
	}
	for _, invalid := range []struct {
		target string
		name   string
	}{
		{target: "header", name: headerName},
		{target: HeaderRecordName, name: strings.ToUpper(headerName)},
		{target: HeaderRecordName, name: HeaderUpdateTemporaryPrefix + strings.Repeat("0", encodedSHA256Characters)},
		{target: fileTarget.Name(), name: target.String() + ".update-" + nonce.String()},
	} {
		if _, err := ParseRecordUpdateTemporaryName(invalid.target, invalid.name); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("invalid temporary %q/%q error = %v", invalid.target, invalid.name, err)
		}
	}
}

func TestHeaderUpdateTemporaryReducerAcceptsOnlyDeterministicGenerationCuts(t *testing.T) {
	namespace := testSessionAuthority(t, SessionActive).NamespaceAuthority()
	nonce := identity32[UpdateNonce](0xab)
	name, err := RecordUpdateTemporaryName(HeaderRecordName, nonce)
	if err != nil {
		t.Fatal(err)
	}
	classified := ClassifyHeaderUpdateTemporaryName(name)
	if classified.Classification() != HeaderUpdateTemporaryCanonical || classified.Nonce() != nonce {
		t.Fatalf("classified header temporary = %+v", classified)
	}

	next, err := namespace.WithLifecycle(SessionPausing)
	if err != nil {
		t.Fatal(err)
	}
	initialHeader := namespace.Header()
	nextHeader := next.Header()
	for _, test := range []struct {
		name      string
		entry     UpdateTemporaryEntryObservation
		candidate *Header
		want      HeaderUpdateTemporaryAction
	}{
		{name: "partial-write", entry: UpdateTemporaryEntryRegular, want: HeaderUpdateTemporaryRemoveAndSyncSession},
		{name: "initial-link", entry: UpdateTemporaryEntryRegular, candidate: &initialHeader, want: HeaderUpdateTemporaryRemoveAndSyncSession},
		{name: "pre-replace", entry: UpdateTemporaryEntryRegular, candidate: &nextHeader, want: HeaderUpdateTemporaryRemoveAndSyncSession},
		{name: "post-replace", entry: UpdateTemporaryEntryMissing, want: HeaderUpdateTemporaryAcceptInstalledHeader},
		{name: "wrong-type", entry: UpdateTemporaryEntryUnsafe, want: HeaderUpdateTemporaryBlockResumeNamespace},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision, reduceErr := ReduceHeaderUpdateTemporary(namespace, classified, test.entry, test.candidate)
			if reduceErr != nil || decision.Action() != test.want {
				t.Fatalf("decision = %+v, %v", decision, reduceErr)
			}
			if test.want == HeaderUpdateTemporaryRemoveAndSyncSession {
				if decision.TemporaryName() != name {
					t.Fatalf("temporary name = %q, want %q", decision.TemporaryName(), name)
				}
				if err := decision.AuthorizeRemoval(namespace, name, UpdateTemporaryEntryRegular); err != nil {
					t.Fatalf("authorize exact removal: %v", err)
				}
			}
		})
	}
	later, err := next.WithLifecycle(SessionPaused)
	if err != nil {
		t.Fatal(err)
	}
	staleDecision, err := ReduceHeaderUpdateTemporary(
		later, classified, UpdateTemporaryEntryRegular, &nextHeader,
	)
	if err != nil || staleDecision.Action() != HeaderUpdateTemporaryRemoveAndSyncSession {
		t.Fatalf("stale header decision = %+v, %v", staleDecision, err)
	}

	sameGenerationDivergence := namespace.Header()
	sameGenerationDivergence.lifecycle = SessionPausing
	skippedGeneration := next.Header()
	skippedGeneration.stateGeneration++
	otherSession := testSessionAuthorityForSelectionAndID(
		t, testSelection(t, 10), SessionActive, identity16[transfer.OutputSessionID](0xee),
	).NamespaceAuthority().Header()
	for _, conflicting := range []Header{sameGenerationDivergence, skippedGeneration, otherSession} {
		decision, reduceErr := ReduceHeaderUpdateTemporary(
			namespace, classified, UpdateTemporaryEntryRegular, &conflicting,
		)
		if reduceErr != nil || decision.Action() != HeaderUpdateTemporaryBlockResumeNamespace {
			t.Fatalf("conflicting header decision = %+v, %v", decision, reduceErr)
		}
	}
	malformed := ClassifyHeaderUpdateTemporaryName(strings.ToUpper(name))
	decision, err := ReduceHeaderUpdateTemporary(namespace, malformed, UpdateTemporaryEntryRegular, nil)
	if err != nil || decision.Action() != HeaderUpdateTemporaryBlockResumeNamespace {
		t.Fatalf("malformed header decision = %+v, %v", decision, err)
	}
}

func TestUpdateTemporaryClassificationChoosesSmallestAttentionScope(t *testing.T) {
	namespace := testSessionAuthority(t, SessionActive).NamespaceAuthority()
	target := identity32[LocatorDigest](0x12)
	nonce := identity32[UpdateNonce](0xab)
	canonicalName := UpdateTemporaryName(target, nonce)
	malformedNames := []struct {
		shard string
		name  string
	}{
		{shard: "12", name: strings.ToUpper(canonicalName.Name())},
		{shard: "34", name: canonicalName.Name()},
		{shard: "12", name: target.String() + UpdateTemporarySeparator + strings.Repeat("0", encodedSHA256Characters)},
		{shard: "12", name: target.String() + ".unexpected"},
	}
	for _, test := range malformedNames {
		classified := ClassifyFileShardEntry(test.shard, test.name)
		if classified.Classification() != FileShardEntryMalformedForLocator || classified.Locator() != target {
			t.Fatalf("malformed classification = %+v", classified)
		}
		decision, err := ReduceUpdateTemporary(namespace, classified, UpdateTemporaryEntryRegular, UpdateTargetValid)
		if err != nil || decision.Action() != UpdateTemporaryInstallFileQuarantine || decision.Target() != target ||
			decision.QuarantineReason() != QuarantineUpdateTemporary {
			t.Fatalf("malformed decision = %+v, %v", decision, err)
		}
	}
	opaque := ClassifyFileShardEntry("zz", "not-a-target")
	decision, err := ReduceUpdateTemporary(namespace, opaque, UpdateTemporaryEntryRegular, UpdateTargetValid)
	if err != nil || decision.Action() != UpdateTemporaryMarkSessionNeedsAttention || !decision.Target().IsZero() {
		t.Fatalf("opaque decision = %+v, %v", decision, err)
	}
}

func TestUpdateTemporaryQuarantineDecisionTransitionsOnlyItsBoundTarget(t *testing.T) {
	namespace := testSessionAuthority(t, SessionActive).NamespaceAuthority()
	target := identity32[LocatorDigest](0x12)
	classified := ClassifiedFileShardEntry{
		classification: FileShardEntryMalformedForLocator,
		locator:        target,
	}
	decision, err := ReduceUpdateTemporary(namespace, classified, UpdateTemporaryEntryRegular, UpdateTargetValid)
	if err != nil {
		t.Fatal(err)
	}
	bound := testBoundFileRecord(t, FileWitnessed)
	bound.record.locatorDigest = target
	// The record's digest is derived from its locator and cannot be relabeled to
	// satisfy a scan result, even from within the same shard.
	if _, err := ApplyUpdateTemporaryQuarantine(bound, decision); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("mismatched temporary target error = %v", err)
	}

	canonicalTarget := testBoundFileRecord(t, FileWitnessed)
	name := UpdateTemporaryName(canonicalTarget.Record().LocatorDigest(), identity32[UpdateNonce](0xab))
	canonical := ClassifyFileShardEntry(name.Shard(), strings.ToUpper(name.Name()))
	decision, err = ReduceUpdateTemporary(canonicalTarget.Session().NamespaceAuthority(), canonical, UpdateTemporaryEntryRegular, UpdateTargetValid)
	if err != nil {
		t.Fatal(err)
	}
	quarantined, err := ApplyUpdateTemporaryQuarantine(canonicalTarget, decision)
	if err != nil || quarantined.Record().Phase() != FileQuarantined ||
		quarantined.Record().QuarantineReason() != QuarantineUpdateTemporary {
		t.Fatalf("temporary quarantine = %+v, %v", quarantined, err)
	}
	reapplied, err := ApplyUpdateTemporaryQuarantine(quarantined, decision)
	if err != nil || reapplied.Record().StateGeneration() != quarantined.Record().StateGeneration() ||
		reapplied.Record().QuarantineReason() != QuarantineUpdateTemporary {
		t.Fatalf("restarted temporary quarantine = %+v, %v", reapplied, err)
	}
	if _, err := ApplyUpdateTemporaryQuarantine(canonicalTarget, UpdateTemporaryDecision{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero temporary decision error = %v", err)
	}
	otherSession := testSessionAuthorityForSelectionAndID(
		t, testSelection(t, 10), SessionActive, identity16[transfer.OutputSessionID](0xee),
	)
	otherRecord := canonicalTarget
	otherRecord.session = otherSession
	otherRecord.record.sessionID = otherSession.Header().SessionID()
	if !otherRecord.valid() {
		t.Fatal("cross-session test record is invalid")
	}
	if _, err := ApplyUpdateTemporaryQuarantine(otherRecord, decision); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("cross-session temporary decision error = %v", err)
	}
}

func TestUpdateTemporaryRemovalRequiresValidatedInstalledTargetAndExactRegularEntry(t *testing.T) {
	installedTarget := testBoundFileRecord(t, FileWitnessed)
	namespace := installedTarget.Session().NamespaceAuthority()
	target := installedTarget.Record().LocatorDigest()
	name := UpdateTemporaryName(target, identity32[UpdateNonce](0xab))
	classified := ClassifyFileShardEntry(name.Shard(), name.Name())
	for entry := UpdateTemporaryEntryRegular; entry <= UpdateTemporaryEntryUnsafe; entry++ {
		for installed := UpdateTargetMissing; installed <= UpdateTargetInvalid; installed++ {
			decision, err := ReduceUpdateTemporary(namespace, classified, entry, installed)
			if err != nil {
				t.Fatal(err)
			}
			canRemove := entry == UpdateTemporaryEntryRegular && installed == UpdateTargetValid
			if canRemove != (decision.Action() == UpdateTemporaryRemoveAndSyncShard) {
				t.Fatalf("entry/target %d/%d decision = %+v", entry, installed, decision)
			}
			if canRemove {
				if decision.TemporaryName() != name ||
					decision.AuthorizeRemoval(installedTarget, name.Shard(), name.Name(), entry) != nil {
					t.Fatalf("removal did not retain exact temporary identity: %+v", decision)
				}
			}
			if !canRemove && installed == UpdateTargetValid && decision.Action() != UpdateTemporaryInstallFileQuarantine {
				t.Fatalf("valid target quarantine %d/%d = %+v", entry, installed, decision)
			}
			if installed != UpdateTargetValid && decision.Action() != UpdateTemporaryRetainLocatorQuarantine {
				t.Fatalf("unbound target quarantine %d/%d = %+v", entry, installed, decision)
			}
		}
	}
	if _, err := ReduceUpdateTemporary(namespace, ClassifiedFileShardEntry{}, UpdateTemporaryEntryRegular, UpdateTargetValid); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid classification error = %v", err)
	}
	if _, err := ReduceUpdateTemporary(SessionNamespaceAuthority{}, classified, UpdateTemporaryEntryRegular, UpdateTargetValid); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid namespace error = %v", err)
	}
	decision, err := ReduceUpdateTemporary(namespace, classified, UpdateTemporaryEntryRegular, UpdateTargetValid)
	if err != nil {
		t.Fatal(err)
	}
	replacement := UpdateTemporaryName(target, identity32[UpdateNonce](0xcd))
	otherSession := testSessionAuthorityForSelectionAndID(
		t, testSelection(t, 10), SessionActive, identity16[transfer.OutputSessionID](0xee),
	)
	otherInstalledTarget := installedTarget
	otherInstalledTarget.session = otherSession
	otherInstalledTarget.record.sessionID = otherSession.Header().SessionID()
	if !otherInstalledTarget.valid() {
		t.Fatal("cross-session installed target fixture is invalid")
	}
	for _, invalid := range []struct {
		installedTarget BoundFileRecord
		shard           string
		name            string
		entry           UpdateTemporaryEntryObservation
	}{
		{installedTarget: installedTarget, shard: replacement.Shard(), name: replacement.Name(), entry: UpdateTemporaryEntryRegular},
		{installedTarget: installedTarget, shard: name.Shard(), name: name.Name(), entry: UpdateTemporaryEntryUnsafe},
		{installedTarget: otherInstalledTarget, shard: name.Shard(), name: name.Name(), entry: UpdateTemporaryEntryRegular},
		{installedTarget: BoundFileRecord{}, shard: name.Shard(), name: name.Name(), entry: UpdateTemporaryEntryRegular},
	} {
		if err := decision.AuthorizeRemoval(invalid.installedTarget, invalid.shard, invalid.name, invalid.entry); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("replacement removal authority error = %v", err)
		}
	}

	// The installed generation is authoritative. A stale temporary is never read
	// or promoted, so any currently valid installed generation authorizes cleanup.
	for _, generation := range []uint64{
		installedTarget.Record().StateGeneration() - 1,
		installedTarget.Record().StateGeneration(),
		installedTarget.Record().StateGeneration() + 1,
	} {
		current := installedTarget
		current.record.stateGeneration = generation
		if !current.valid() {
			t.Fatalf("installed generation %d fixture is invalid", generation)
		}
		if err := decision.AuthorizeRemoval(current, name.Shard(), name.Name(), UpdateTemporaryEntryRegular); err != nil {
			t.Fatalf("installed generation %d did not authorize stale temporary cleanup: %v", generation, err)
		}
	}
}

func TestUpdateTemporaryPostRenameUncertaintyTrustsOnlyReopenedInstalledTarget(t *testing.T) {
	installedTarget := testBoundFileRecord(t, FileWitnessed)
	namespace := installedTarget.Session().NamespaceAuthority()
	name := UpdateTemporaryName(installedTarget.Record().LocatorDigest(), identity32[UpdateNonce](0xab))
	classified := ClassifyFileShardEntry(name.Shard(), name.Name())

	installed, err := ReduceUpdateTemporary(namespace, classified, UpdateTemporaryEntryMissing, UpdateTargetValid)
	if err != nil || installed.Action() != UpdateTemporaryAcceptInstalledTarget ||
		installed.Target() != installedTarget.Record().LocatorDigest() || installed.TemporaryName() != name {
		t.Fatalf("post-rename installed cut = %+v, %v", installed, err)
	}
	if err := installed.AuthorizeRemoval(installedTarget, name.Shard(), name.Name(), UpdateTemporaryEntryMissing); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("missing post-rename temporary was authorized for unlink: %v", err)
	}
	for _, target := range []UpdateTargetObservation{UpdateTargetMissing, UpdateTargetInvalid} {
		decision, reduceErr := ReduceUpdateTemporary(namespace, classified, UpdateTemporaryEntryMissing, target)
		if reduceErr != nil || decision.Action() != UpdateTemporaryRetainLocatorQuarantine ||
			decision.QuarantineReason() != QuarantineUpdateTemporary {
			t.Fatalf("uncertain post-rename target %d = %+v, %v", target, decision, reduceErr)
		}
	}
}
