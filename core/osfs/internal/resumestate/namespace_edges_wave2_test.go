package resumestate

import (
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

func TestResumestateWave2RejectsNonAuthoritativeNamespaceSpellings(t *testing.T) {
	invalidHex := strings.Repeat("g", encodedSHA256Characters)
	zeroIdentity := strings.Repeat("0", encodedSHA256Characters)

	for name, want := range map[string]IntentNamespaceClassification{
		invalidHex:   IntentNamespaceOpaque,
		zeroIdentity: IntentNamespaceOpaque,
	} {
		classified := ClassifyIntentNamespaceName(name)
		if classified.Classification() != want || classified.Intent() != ([32]byte{}) {
			t.Fatalf("intent namespace %q = %+v", name, classified)
		}
		if _, err := ParseIntentNamespaceName(name); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("intent namespace %q parse error = %v", name, err)
		}
	}
}

func TestResumestateWave2FileShardClassificationRetainsOnlyValidLocatorAuthority(t *testing.T) {
	tests := []struct {
		name  string
		shard string
		entry string
		want  FileShardEntryClassification
	}{
		{name: "invalid-encoded-locator", shard: "gg", entry: strings.Repeat("g", encodedSHA256Characters), want: FileShardEntryOpaque},
		{name: "zero-locator", shard: "00", entry: strings.Repeat("0", encodedSHA256Characters), want: FileShardEntryOpaque},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classified := ClassifyFileShardEntry(test.shard, test.entry)
			if classified.Classification() != test.want || !classified.Locator().IsZero() || !classified.Nonce().IsZero() {
				t.Fatalf("classification = %+v", classified)
			}
		})
	}
}

func TestResumestateWave2TemporaryGrammarAndReducersRejectForgedIdentity(t *testing.T) {
	namespace := testSessionAuthority(t, SessionActive).NamespaceAuthority()
	bound := testBoundFileRecord(t, FileWitnessed)
	target := bound.Record().LocatorDigest()

	if _, err := RecordUpdateTemporaryName(HeaderRecordName, UpdateNonce{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero nonce temporary error = %v", err)
	}
	malformedHeader := HeaderUpdateTemporaryPrefix + strings.Repeat("g", encodedSHA256Characters)
	classifiedHeader := ClassifyHeaderUpdateTemporaryName(malformedHeader)
	if classifiedHeader.Classification() != HeaderUpdateTemporaryMalformed {
		t.Fatalf("malformed header classification = %+v", classifiedHeader)
	}
	forgedHeaderDecision := HeaderUpdateTemporaryDecision{
		action: HeaderUpdateTemporaryRemoveAndSyncSession, temporary: malformedHeader, namespace: namespace,
	}
	if err := forgedHeaderDecision.AuthorizeRemoval(namespace, malformedHeader, UpdateTemporaryEntryRegular); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("forged header removal error = %v", err)
	}

	invalidObservations := []ClassifiedFileShardEntry{
		{classification: FileShardEntryOpaque, locator: target},
		{classification: FileShardEntryMalformedForLocator},
		{classification: FileShardEntryMalformedForLocator, locator: target, nonce: identity32[UpdateNonce](0x44)},
		{classification: FileShardEntryUpdateTemporary, locator: target},
		{classification: FileShardEntryRecord, locator: target},
	}
	for _, classified := range invalidObservations {
		if _, err := ReduceUpdateTemporary(
			namespace, classified, UpdateTemporaryEntryRegular, UpdateTargetValid,
		); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("forged classification %+v error = %v", classified, err)
		}
	}

	malformedName := ShardedName{
		shard: FileRecordName(target).Shard(),
		name:  target.String() + ".unexpected",
	}
	forgedFileDecision := UpdateTemporaryDecision{
		action: UpdateTemporaryRemoveAndSyncShard, target: target, temporary: malformedName,
		namespace: bound.Session().NamespaceAuthority(),
	}
	if err := forgedFileDecision.AuthorizeRemoval(
		bound, malformedName.Shard(), malformedName.Name(), UpdateTemporaryEntryRegular,
	); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("forged file removal error = %v", err)
	}
}

func TestResumestateWave2AuthorityRejectsUnboundAndMutatedInputs(t *testing.T) {
	authority := testSessionAuthority(t, SessionActive)
	bound := testBoundFileRecord(t, FileWitnessed)
	recordName := FileRecordName(bound.Record().LocatorDigest())

	if _, err := BindSessionAuthority(
		authority.Control(), authority.Header(), transfer.OutputSelection{},
		authority.IntentDirectory(), authority.SessionDirectory(),
	); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("mismatched selection error = %v", err)
	}
	if _, err := BindSessionAuthority(
		Control{}, Header{}, transfer.OutputSelection{}, "intent", "session",
	); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unbound namespace error = %v", err)
	}
	if _, err := (SessionAuthority{}).WithLifecycle(SessionPaused); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unbound lifecycle error = %v", err)
	}
	if _, err := (SessionNamespaceAuthority{}).WithLifecycle(SessionPaused); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unbound namespace lifecycle error = %v", err)
	}

	intentWithoutFiles := authority
	intentWithoutFiles.liveIntentDigest = identity32[transfer.TransferIntentDigest](0x71)
	if intentWithoutFiles.valid() {
		t.Fatal("live intent without live files was valid")
	}
	mismatchedIndexes := authority
	mismatchedIndexes.liveFilesByKey = map[LiveFileKey]LiveFileSelection{}
	mismatchedIndexes.liveKeysByLocator = map[string]LiveFileKey{"orphan": {}}
	if mismatchedIndexes.valid() {
		t.Fatal("mismatched live indexes were valid")
	}
	if cloneLiveFileMap(nil) != nil || cloneLiveFileLocatorMap(nil) != nil {
		t.Fatal("nil live indexes did not preserve nil")
	}

	if _, err := BindFileRecord(SessionAuthority{}, recordName.Shard(), recordName.Name(), bound.Record()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unbound file record error = %v", err)
	}
	if _, err := BindFileRecord(bound.Session(), "zz", recordName.Name(), bound.Record()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("misplaced file record error = %v", err)
	}
	if _, err := NewFileRecord(FileRecordSpec{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unbound file specification error = %v", err)
	}

	if _, err := (ResumableFileAuthority{}).WithCheckpoint(1, content.RangeSet{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unbound checkpoint error = %v", err)
	}
	if _, err := ReduceResumableFileRecovery(ResumableFileAuthority{}, FileObservation{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unbound resumable recovery error = %v", err)
	}
	if _, err := reduceFileRecovery(BoundFileRecord{}, FileObservation{}, false); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unbound file recovery error = %v", err)
	}
	if _, err := PreparePublication(ResumableFileAuthority{}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unbound publication error = %v", err)
	}
	if _, err := PreparePublishedRetirement(BoundFileRecord{}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unbound published retirement error = %v", err)
	}
	if _, err := PrepareIsolatedRetirement(BoundFileRecord{}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unbound isolated retirement error = %v", err)
	}
	if _, err := PrepareInvalidatedRevisionRetirement(BoundFileRecord{}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unbound invalidated retirement error = %v", err)
	}

	if _, err := NewBootstrapIntent(authority.Control(), BootstrapNonce{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero bootstrap nonce error = %v", err)
	}
}
