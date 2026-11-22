package content

import (
	"bytes"
	"errors"
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

func TestCanonicalRevisionEvidenceSeparatesEveryField(t *testing.T) {
	baselineEvidence := knownRevisionEvidence(t)
	baseline := canonicalRevisionEvidence(baselineEvidence)

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
		if bytes.Equal(canonicalRevisionEvidence(mutation), baseline) {
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

func TestRevisionComparisonMarkerPreservesTypedErrors(t *testing.T) {
	unavailable := WithRevisionComparison(ErrRevisionStale, RevisionComparisonUnavailable)
	if !errors.Is(unavailable, ErrRevisionStale) || RevisionComparisonOf(unavailable) != RevisionComparisonUnavailable {
		t.Fatalf("unavailable marker=%v comparison=%v", unavailable, RevisionComparisonOf(unavailable))
	}
	if RevisionComparisonOf(ErrRevisionStale) != RevisionComparisonMismatch ||
		RevisionComparisonOf(errors.New("transient")) != RevisionComparisonUnavailable ||
		RevisionComparisonOf(nil) != RevisionComparisonMetadataAgreement {
		t.Fatal("default comparison classification changed")
	}
}
