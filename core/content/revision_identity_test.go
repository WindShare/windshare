package content

import (
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func knownRevisionEvidence(t *testing.T) RevisionEvidence {
	t.Helper()
	share := catalogID[catalog.ShareInstance](1)
	file := catalogID[catalog.FileID](17)
	source, err := catalog.NewSourceIdentity([]byte("source-token"))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := catalog.NewVersionCandidate([]byte("version-token"))
	if err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(-123_456_789, 987_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := NewRevisionEvidence(share, file, source, candidate, 0x0102_0304_0506, modified, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func knownRevisionKey() RevisionIdentityKey {
	var key RevisionIdentityKey
	for index := range key {
		key[index] = byte(index + 1)
	}
	return key
}

func TestHMACRevisionIdentityKnownAnswerAndFieldSeparation(t *testing.T) {
	deriver, err := NewHMACRevisionIdentityDeriver(knownRevisionKey())
	if err != nil {
		t.Fatal(err)
	}
	baselineEvidence := knownRevisionEvidence(t)
	baseline, err := deriver.DeriveRevision(baselineEvidence)
	if err != nil {
		t.Fatal(err)
	}
	const expectedHex = "c7d553ebb12bd110c3893c6ba43897f5"
	if got := hex.EncodeToString(baseline[:]); got != expectedHex {
		t.Fatalf("derived revision=%s want=%s", got, expectedHex)
	}

	otherShare := baselineEvidence.shareInstance
	otherShare[0]++
	otherFile := baselineEvidence.fileID
	otherFile[0]++
	otherSource, _ := catalog.NewSourceIdentity([]byte("source-token-2"))
	otherCandidate, _ := catalog.NewVersionCandidate([]byte("version-token-2"))
	otherModified, _ := catalog.NewModifiedTime(-123_456_788, 987_000_000, catalog.TimePrecisionMilliseconds)
	mutations := []RevisionEvidence{
		mustRevisionEvidence(t, otherShare, baselineEvidence.fileID, baselineEvidence.sourceIdentity, baselineEvidence.versionCandidate, baselineEvidence.expectedSize, baselineEvidence.modifiedTime, baselineEvidence.chunkSize),
		mustRevisionEvidence(t, baselineEvidence.shareInstance, otherFile, baselineEvidence.sourceIdentity, baselineEvidence.versionCandidate, baselineEvidence.expectedSize, baselineEvidence.modifiedTime, baselineEvidence.chunkSize),
		mustRevisionEvidence(t, baselineEvidence.shareInstance, baselineEvidence.fileID, otherSource, baselineEvidence.versionCandidate, baselineEvidence.expectedSize, baselineEvidence.modifiedTime, baselineEvidence.chunkSize),
		mustRevisionEvidence(t, baselineEvidence.shareInstance, baselineEvidence.fileID, baselineEvidence.sourceIdentity, otherCandidate, baselineEvidence.expectedSize, baselineEvidence.modifiedTime, baselineEvidence.chunkSize),
		mustRevisionEvidence(t, baselineEvidence.shareInstance, baselineEvidence.fileID, baselineEvidence.sourceIdentity, baselineEvidence.versionCandidate, baselineEvidence.expectedSize+1, baselineEvidence.modifiedTime, baselineEvidence.chunkSize),
		mustRevisionEvidence(t, baselineEvidence.shareInstance, baselineEvidence.fileID, baselineEvidence.sourceIdentity, baselineEvidence.versionCandidate, baselineEvidence.expectedSize, otherModified, baselineEvidence.chunkSize),
		mustRevisionEvidence(t, baselineEvidence.shareInstance, baselineEvidence.fileID, baselineEvidence.sourceIdentity, baselineEvidence.versionCandidate, baselineEvidence.expectedSize, baselineEvidence.modifiedTime, baselineEvidence.chunkSize*2),
	}
	for index, mutation := range mutations {
		derived, err := deriver.DeriveRevision(mutation)
		if err != nil {
			t.Fatalf("mutation %d: %v", index, err)
		}
		if derived == baseline {
			t.Fatalf("mutation %d did not separate its canonical field", index)
		}
	}
}

func mustRevisionEvidence(
	t *testing.T,
	share catalog.ShareInstance,
	file catalog.FileID,
	source catalog.SourceIdentity,
	candidate catalog.VersionCandidate,
	size uint64,
	modified catalog.ModifiedTime,
	chunk uint32,
) RevisionEvidence {
	t.Helper()
	evidence, err := NewRevisionEvidence(share, file, source, candidate, size, modified, chunk)
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func TestRevisionIdentityDeriverDestroyIsConcurrentAndPermanent(t *testing.T) {
	if _, err := NewHMACRevisionIdentityDeriver(RevisionIdentityKey{}); err == nil {
		t.Fatal("zero revision identity key was accepted")
	}
	deriver, err := NewHMACRevisionIdentityDeriver(knownRevisionKey())
	if err != nil {
		t.Fatal(err)
	}
	evidence := knownRevisionEvidence(t)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			_, deriveErr := deriver.DeriveRevision(evidence)
			if deriveErr != nil && !errors.Is(deriveErr, ErrRevisionIdentityDestroyed) {
				t.Errorf("concurrent derive error=%v", deriveErr)
			}
		})
	}
	deriver.Destroy()
	workers.Wait()
	deriver.Destroy()
	if _, err := deriver.DeriveRevision(evidence); !errors.Is(err, ErrRevisionIdentityDestroyed) {
		t.Fatalf("derive after destroy=%v", err)
	}
}

func TestRevisionComparisonMarkerPreservesTypedErrors(t *testing.T) {
	unavailable := WithRevisionComparison(ErrRevisionStale, RevisionComparisonUnavailable)
	if !errors.Is(unavailable, ErrRevisionStale) || RevisionComparisonOf(unavailable) != RevisionComparisonUnavailable {
		t.Fatalf("unavailable marker=%v comparison=%v", unavailable, RevisionComparisonOf(unavailable))
	}
	if RevisionComparisonOf(ErrRevisionStale) != RevisionComparisonMismatch ||
		RevisionComparisonOf(errors.New("transient")) != RevisionComparisonUnavailable ||
		RevisionComparisonOf(nil) != RevisionComparisonMatch {
		t.Fatal("default comparison classification changed")
	}
}

func TestRevisionMetadataBudgetRetainsUntilReservationRelease(t *testing.T) {
	budget := testRevisionMetadataBudget(t, 1)
	reservation, err := budget.reserveInvalidation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := budget.reserveInvalidation(); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over-budget reservation=%v", err)
	}
	if got := budget.Snapshot(); got != (RevisionMetadataSnapshot{Capacity: 1, Used: 1}) {
		t.Fatalf("budget snapshot=%+v", got)
	}
	reservation.release()
	if got := budget.Snapshot(); got.Used != 0 {
		t.Fatalf("released budget snapshot=%+v", got)
	}
}
