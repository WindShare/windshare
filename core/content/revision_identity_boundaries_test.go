package content

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestRevisionEvidenceRejectsIdentityAndGeometryBoundaries(t *testing.T) {
	baseline := knownRevisionEvidence(t)
	tests := []struct {
		name    string
		mutate  func(*RevisionEvidence)
		wantErr error
	}{
		{name: "zero share", mutate: func(evidence *RevisionEvidence) { evidence.shareInstance = catalog.ShareInstance{} }},
		{name: "zero file", mutate: func(evidence *RevisionEvidence) { evidence.fileID = catalog.FileID{} }},
		{name: "zero source", mutate: func(evidence *RevisionEvidence) { evidence.sourceIdentity = catalog.SourceIdentity{} }},
		{name: "zero version", mutate: func(evidence *RevisionEvidence) { evidence.versionCandidate = catalog.VersionCandidate{} }},
		{name: "portable size exceeded", mutate: func(evidence *RevisionEvidence) { evidence.expectedSize = catalog.MaxFileSize + 1 }},
		{name: "zero chunk", mutate: func(evidence *RevisionEvidence) { evidence.chunkSize = 0 }, wantErr: ErrInvalidGeometry},
		{name: "chunk above maximum", mutate: func(evidence *RevisionEvidence) { evidence.chunkSize = catalog.MaxChunkSize + 1 }, wantErr: ErrInvalidGeometry},
		{name: "non power of two chunk", mutate: func(evidence *RevisionEvidence) { evidence.chunkSize = catalog.MinChunkSize + 1 }, wantErr: ErrInvalidGeometry},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := baseline
			test.mutate(&input)
			got, err := NewRevisionEvidence(
				input.shareInstance,
				input.fileID,
				input.sourceIdentity,
				input.versionCandidate,
				input.expectedSize,
				input.modifiedTime,
				input.chunkSize,
			)
			if err == nil {
				t.Fatal("invalid evidence was accepted")
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if got != (RevisionEvidence{}) {
				t.Fatalf("rejected evidence retained authority: %+v", got)
			}
		})
	}
}

func TestRevisionEvidenceImmutableProjections(t *testing.T) {
	evidence := knownRevisionEvidence(t)
	projections := []struct {
		name string
		got  any
		want any
	}{
		{name: "share", got: evidence.ShareInstance(), want: evidence.shareInstance},
		{name: "file", got: evidence.FileID(), want: evidence.fileID},
		{name: "source", got: evidence.SourceIdentity(), want: evidence.sourceIdentity},
		{name: "version", got: evidence.VersionCandidate(), want: evidence.versionCandidate},
		{name: "size", got: evidence.ExpectedSize(), want: evidence.expectedSize},
		{name: "modified", got: evidence.ModifiedTime(), want: evidence.modifiedTime},
		{name: "chunk", got: evidence.ChunkSize(), want: evidence.chunkSize},
	}
	for _, projection := range projections {
		t.Run(projection.name, func(t *testing.T) {
			if !reflect.DeepEqual(projection.got, projection.want) {
				t.Fatalf("projection = %#v, want %#v", projection.got, projection.want)
			}
		})
	}

	share := evidence.ShareInstance()
	share[0]++
	file := evidence.FileID()
	file[0]++
	source := evidence.SourceIdentity().Bytes()
	source[0]++
	version := evidence.VersionCandidate().Bytes()
	version[0]++
	if evidence.ShareInstance() != evidence.shareInstance || evidence.FileID() != evidence.fileID ||
		!slices.Equal(evidence.SourceIdentity().Bytes(), evidence.sourceIdentity.Bytes()) ||
		!slices.Equal(evidence.VersionCandidate().Bytes(), evidence.versionCandidate.Bytes()) {
		t.Fatal("mutating projected evidence changed the frozen identity inputs")
	}
}

func TestRevisionIdentityDeriverRejectsMalformedEvidenceAndLifecycleBoundaries(t *testing.T) {
	var nilDeriver *HMACRevisionIdentityDeriver
	// Destroy is intentionally nil-safe so cleanup paths do not need conditional authority handling.
	nilDeriver.Destroy()

	alive, err := NewHMACRevisionIdentityDeriver(knownRevisionKey())
	if err != nil {
		t.Fatal(err)
	}
	destroyed, err := NewHMACRevisionIdentityDeriver(knownRevisionKey())
	if err != nil {
		t.Fatal(err)
	}
	destroyed.Destroy()
	t.Cleanup(alive.Destroy)

	invalidGeometry := knownRevisionEvidence(t)
	invalidGeometry.chunkSize = 0
	tests := []struct {
		name     string
		deriver  *HMACRevisionIdentityDeriver
		evidence RevisionEvidence
		wantErr  error
	}{
		{name: "nil deriver", deriver: nilDeriver, evidence: knownRevisionEvidence(t)},
		{name: "zero evidence", deriver: alive, evidence: RevisionEvidence{}},
		{name: "invalid geometry", deriver: alive, evidence: invalidGeometry, wantErr: ErrInvalidGeometry},
		{name: "destroyed deriver", deriver: destroyed, evidence: knownRevisionEvidence(t), wantErr: ErrRevisionIdentityDestroyed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			revision, deriveErr := test.deriver.DeriveRevision(test.evidence)
			if deriveErr == nil {
				t.Fatal("invalid derivation was accepted")
			}
			if test.wantErr != nil && !errors.Is(deriveErr, test.wantErr) {
				t.Fatalf("error = %v, want %v", deriveErr, test.wantErr)
			}
			if !revision.IsZero() {
				t.Fatalf("failed derivation returned revision %x", revision)
			}
		})
	}
}

func TestRevisionMetadataBudgetBoundariesAndIdempotentRelease(t *testing.T) {
	if budget, err := NewRevisionMetadataBudget(0); err == nil || budget != nil {
		t.Fatalf("zero-capacity budget = %v, %v", budget, err)
	}
	var nilBudget *RevisionMetadataBudget
	if got := nilBudget.Snapshot(); got != (RevisionMetadataSnapshot{}) {
		t.Fatalf("nil snapshot = %+v", got)
	}
	if reservation, err := nilBudget.reserveInvalidation(); err == nil || reservation != nil {
		t.Fatalf("nil reservation = %v, %v", reservation, err)
	}
	var nilReservation *revisionMetadataReservation
	nilReservation.release()

	budget := testRevisionMetadataBudget(t, 1)
	reservation, err := budget.reserveInvalidation()
	if err != nil {
		t.Fatal(err)
	}
	if got := budget.Snapshot(); got != (RevisionMetadataSnapshot{Capacity: 1, Used: 1}) {
		t.Fatalf("full budget snapshot = %+v", got)
	}
	if extra, reserveErr := budget.reserveInvalidation(); !errors.Is(reserveErr, ErrQuotaExceeded) || extra != nil {
		t.Fatalf("over-capacity reservation = %v, %v", extra, reserveErr)
	}
	reservation.release()
	reservation.release()
	if got := budget.Snapshot(); got != (RevisionMetadataSnapshot{Capacity: 1}) {
		t.Fatalf("idempotent release snapshot = %+v", got)
	}
	reused, err := budget.reserveInvalidation()
	if err != nil {
		t.Fatalf("released capacity was not reusable: %v", err)
	}
	reused.release()
}

type revisionBoundaryClassifiedError struct {
	comparison RevisionComparison
}

func (e revisionBoundaryClassifiedError) Error() string { return "boundary comparison" }

func (e revisionBoundaryClassifiedError) RevisionComparison() RevisionComparison {
	return e.comparison
}

func TestRevisionComparisonWrappingBoundaries(t *testing.T) {
	sentinel := errors.New("provider failure")
	wrapping := []struct {
		name       string
		err        error
		comparison RevisionComparison
		wantSame   bool
	}{
		{name: "nil", err: nil, comparison: RevisionComparisonMismatch, wantSame: true},
		{name: "unknown", err: sentinel, comparison: RevisionComparisonUnknown, wantSame: true},
		{name: "match", err: sentinel, comparison: RevisionComparisonMatch, wantSame: true},
		{name: "mismatch", err: sentinel, comparison: RevisionComparisonMismatch},
		{name: "unavailable", err: sentinel, comparison: RevisionComparisonUnavailable},
	}
	for _, test := range wrapping {
		t.Run(test.name, func(t *testing.T) {
			wrapped := WithRevisionComparison(test.err, test.comparison)
			if test.wantSame && wrapped != test.err {
				t.Fatalf("wrapper changed error identity: %v", wrapped)
			}
			if test.err != nil && !errors.Is(wrapped, test.err) {
				t.Fatalf("wrapper lost provider error: %v", wrapped)
			}
			if test.err != nil && wrapped.Error() != test.err.Error() {
				t.Fatalf("wrapper changed provider message: %q", wrapped.Error())
			}
		})
	}

	classifications := []struct {
		name string
		err  error
		want RevisionComparison
	}{
		{name: "nil is match", err: nil, want: RevisionComparisonMatch},
		{name: "source drift", err: ErrSourceDrift, want: RevisionComparisonMismatch},
		{name: "stale revision", err: ErrRevisionStale, want: RevisionComparisonMismatch},
		{name: "explicit mismatch", err: WithRevisionComparison(sentinel, RevisionComparisonMismatch), want: RevisionComparisonMismatch},
		{name: "explicit unavailable", err: WithRevisionComparison(sentinel, RevisionComparisonUnavailable), want: RevisionComparisonUnavailable},
		{name: "invalid marker", err: revisionBoundaryClassifiedError{comparison: RevisionComparisonMatch}, want: RevisionComparisonUnavailable},
		{name: "unclassified failure", err: sentinel, want: RevisionComparisonUnavailable},
	}
	for _, test := range classifications {
		t.Run(test.name, func(t *testing.T) {
			if got := RevisionComparisonOf(test.err); got != test.want {
				t.Fatalf("comparison = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRevisionTraceImmutableProjections(t *testing.T) {
	trace := RevisionTrace{
		stage:         RevisionTraceStageMismatchInvalidation,
		cause:         RevisionTraceCauseModifiedTime,
		shareInstance: catalogID[catalog.ShareInstance](1),
		fileID:        catalogID[catalog.FileID](2),
		fileRevision:  contentID[FileRevision](3),
	}
	projections := []struct {
		name string
		got  any
		want any
	}{
		{name: "stage", got: trace.Stage(), want: trace.stage},
		{name: "cause", got: trace.Cause(), want: trace.cause},
		{name: "share", got: trace.ShareInstance(), want: trace.shareInstance},
		{name: "file", got: trace.FileID(), want: trace.fileID},
		{name: "revision", got: trace.FileRevision(), want: trace.fileRevision},
	}
	for _, projection := range projections {
		t.Run(projection.name, func(t *testing.T) {
			if !reflect.DeepEqual(projection.got, projection.want) {
				t.Fatalf("projection = %#v, want %#v", projection.got, projection.want)
			}
		})
	}

	share := trace.ShareInstance()
	share[0]++
	file := trace.FileID()
	file[0]++
	revision := trace.FileRevision()
	revision[0]++
	if trace.ShareInstance() != trace.shareInstance || trace.FileID() != trace.fileID || trace.FileRevision() != trace.fileRevision {
		t.Fatal("mutating trace projections changed the recorded event")
	}
}

type failOnceRevisionDeriver struct {
	mu       sync.Mutex
	delegate RevisionIdentityDeriver
	failure  error
	failed   bool
}

func (d *failOnceRevisionDeriver) DeriveRevision(evidence RevisionEvidence) (FileRevision, error) {
	d.mu.Lock()
	if !d.failed {
		d.failed = true
		failure := d.failure
		d.mu.Unlock()
		return FileRevision{}, failure
	}
	d.mu.Unlock()
	return d.delegate.DeriveRevision(evidence)
}

func TestRevisionStoreDeriverFailureRollsBackAdmissionAndRemainsRetryable(t *testing.T) {
	file, record := fileRecord(t, 1)
	process := generousQuota(t, "process")
	share := generousQuota(t, "share")
	session := generousQuota(t, "session")
	stable := &testStableFile{data: []byte{1}}
	source := &testRevisionSource{files: []*testStableFile{stable}}
	delegate := testRevisionDeriver(t)
	sentinel := errors.New("injected revision derivation failure")
	deriver := &failOnceRevisionDeriver{delegate: delegate, failure: sentinel}
	store, err := NewRevisionStore(RevisionStoreConfig{
		ShareInstance:   catalogID[catalog.ShareInstance](1),
		ChunkSize:       catalog.MinChunkSize,
		Catalog:         testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		Source:          source,
		ProcessQuota:    process,
		ShareQuota:      share,
		LeaseIDs:        &sequenceIDs{},
		RevisionDeriver: deriver,
		MetadataBudget:  testRevisionMetadataBudget(t, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = store.Close()
		}
		delegate.Destroy()
	})

	if _, openErr := store.OpenRevision(context.Background(), file, session); !errors.Is(openErr, sentinel) {
		t.Fatalf("first open error = %v", openErr)
	}
	for _, account := range []*QuotaAccount{process, share, session} {
		if got := account.Snapshot().Used; got != (QuotaUsage{}) {
			t.Fatalf("failed derivation leaked %s quota: %+v", account.Name(), got)
		}
	}
	if source.Calls() != 0 {
		t.Fatalf("derivation failure opened %d physical sources", source.Calls())
	}

	lease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatalf("retry after derivation failure = %v", err)
	}
	if lease.ID().IsZero() || source.Calls() != 1 {
		t.Fatalf("retry did not publish exactly one source-backed lease: lease=%x calls=%d", lease.ID(), source.Calls())
	}
	for _, account := range []*QuotaAccount{process, share, session} {
		if got := account.Snapshot().Used; got != (QuotaUsage{StableHandles: 1, ActiveLeases: 1}) {
			t.Fatalf("successful retry charged %s quota = %+v", account.Name(), got)
		}
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	for _, account := range []*QuotaAccount{process, share, session} {
		if got := account.Snapshot().Used; got != (QuotaUsage{}) {
			t.Fatalf("store close leaked %s quota: %+v", account.Name(), got)
		}
	}
}
