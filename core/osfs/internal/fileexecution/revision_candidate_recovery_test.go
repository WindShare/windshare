package fileexecution

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
)

type revisionCandidateRecoveryHarness struct {
	fixture     executionFixture
	engine      *Engine
	repository  *memoryCheckpointRepository
	platform    *memoryPlatform
	destination *memoryDestination
	key         CheckpointKey
	object      checkpointmodel.ObjectID
	candidate   checkpointmodel.Record
}

func newRevisionCandidateRecoveryHarness(t *testing.T) revisionCandidateRecoveryHarness {
	t.Helper()
	fixture := newExecutionFixture(t, 4)
	repository := &memoryCheckpointRepository{}
	platform := newMemoryPlatform()
	destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	key, err := engine.checkpointKey(fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	object, err := checkpointmodel.ObjectIDFromBytes(
		bytes.Repeat([]byte{0x79}, transfer.OwnedObjectIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := newInitialRecord(key, object)
	if err != nil {
		t.Fatal(err)
	}
	repository.record, repository.present = candidate, true
	return revisionCandidateRecoveryHarness{
		fixture: fixture, engine: engine, repository: repository, platform: platform,
		destination: destination, key: key, object: object, candidate: candidate,
	}
}

type revisionCandidatePlatform struct {
	Platform
	createCalls int
	openCalls   int
	create      func(context.Context, FileDestination, checkpointmodel.ObjectID, uint64) (OwnedFile, OwnedObservation, error)
	open        func(context.Context, checkpointmodel.ObjectID, uint64, bool) (OwnedFile, OwnedObservation, error)
}

func (platform *revisionCandidatePlatform) CreateOwnedFile(
	ctx context.Context,
	destination FileDestination,
	object checkpointmodel.ObjectID,
	exactSize uint64,
) (OwnedFile, OwnedObservation, error) {
	platform.createCalls++
	return platform.create(ctx, destination, object, exactSize)
}

func (platform *revisionCandidatePlatform) OpenOwnedFile(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
	writable bool,
) (OwnedFile, OwnedObservation, error) {
	platform.openCalls++
	return platform.open(ctx, object, exactSize, writable)
}

type revisionCandidateOwnedFile struct {
	*memoryOwnedFile
	syncCalls int
}

func (file *revisionCandidateOwnedFile) Sync() error {
	file.syncCalls++
	return file.memoryOwnedFile.Sync()
}

func TestRecreateInitialCandidateOwnedReadyPromotesAndStartsTransaction(t *testing.T) {
	harness := newRevisionCandidateRecoveryHarness(t)
	absent, err := NewOwnedObservation(harness.object, OwnedAbsent)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := NewOwnedObservation(harness.object, OwnedReady)
	if err != nil {
		t.Fatal(err)
	}
	created := &revisionCandidateOwnedFile{memoryOwnedFile: &memoryOwnedFile{
		object: harness.object,
		data:   &memoryOwnedData{bytes: make([]byte, harness.candidate.ExactSize())},
	}}
	platform := &revisionCandidatePlatform{Platform: harness.platform}
	platform.open = func(context.Context, checkpointmodel.ObjectID, uint64, bool) (OwnedFile, OwnedObservation, error) {
		return nil, absent, nil
	}
	platform.create = func(context.Context, FileDestination, checkpointmodel.ObjectID, uint64) (OwnedFile, OwnedObservation, error) {
		return created, ready, nil
	}
	harness.engine.platform = platform

	start, err := harness.engine.BeginFile(context.Background(), harness.fixture.file, harness.fixture.destination)
	transaction, durable, hasTransaction := start.Transaction()
	if err != nil || !hasTransaction {
		t.Fatalf("recreated candidate start = (transaction=%t, %v)", hasTransaction, err)
	}
	if platform.openCalls != 1 || platform.createCalls != 1 || created.syncCalls != 1 {
		t.Fatalf("recreation calls = (open=%d create=%d sync=%d)", platform.openCalls, platform.createCalls, created.syncCalls)
	}
	if len(harness.repository.stores) != 1 ||
		harness.repository.record.CommitState() != checkpointmodel.CommitVerified ||
		harness.repository.record.RecordID() != harness.candidate.RecordID() {
		t.Fatalf("candidate replacement = stores=%d record=%+v", len(harness.repository.stores), harness.repository.record)
	}
	if !durable.Ranges().IsEmpty() || len(harness.repository.record.VerifiedRanges()) != 0 {
		t.Fatalf("pristine candidate invented verified ranges: durable=%v checkpoint=%v",
			durable.Ranges().Ranges(), harness.repository.record.VerifiedRanges())
	}
	if !bytes.Equal(transaction.Binding().ObjectIdentity().Bytes(), harness.object.Bytes()) {
		t.Fatalf("transaction object = %x, want %x", transaction.Binding().ObjectIdentity().Bytes(), harness.object.Bytes())
	}
}

func TestRecreateInitialCandidateSameObjectCollisionResumes(t *testing.T) {
	harness := newRevisionCandidateRecoveryHarness(t)
	absent, err := NewOwnedObservation(harness.object, OwnedAbsent)
	if err != nil {
		t.Fatal(err)
	}
	collision, err := NewOwnedObservation(harness.object, OwnedObjectCollision)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := NewOwnedObservation(harness.object, OwnedReady)
	if err != nil {
		t.Fatal(err)
	}
	data := &memoryOwnedData{bytes: make([]byte, harness.candidate.ExactSize())}
	reopened := &revisionCandidateOwnedFile{memoryOwnedFile: &memoryOwnedFile{object: harness.object, data: data}}
	harness.platform.objects[harness.object] = data
	platform := &revisionCandidatePlatform{Platform: harness.platform}
	platform.open = func(context.Context, checkpointmodel.ObjectID, uint64, bool) (OwnedFile, OwnedObservation, error) {
		if platform.openCalls == 1 {
			return nil, absent, nil
		}
		return reopened, ready, nil
	}
	platform.create = func(context.Context, FileDestination, checkpointmodel.ObjectID, uint64) (OwnedFile, OwnedObservation, error) {
		return nil, collision, nil
	}
	harness.engine.platform = platform

	start, err := harness.engine.BeginFile(context.Background(), harness.fixture.file, harness.fixture.destination)
	transaction, durable, hasTransaction := start.Transaction()
	if err != nil || !hasTransaction {
		t.Fatalf("same-object collision start = (transaction=%t, %v)", hasTransaction, err)
	}
	if platform.openCalls != 2 || platform.createCalls != 1 || reopened.syncCalls != 1 {
		t.Fatalf("collision recovery calls = (open=%d create=%d sync=%d)", platform.openCalls, platform.createCalls, reopened.syncCalls)
	}
	if harness.repository.record.CommitState() != checkpointmodel.CommitVerified ||
		len(harness.repository.stores) != 1 || !durable.Ranges().IsEmpty() {
		t.Fatalf("collision recovery authority = commit=%d stores=%d durable=%v",
			harness.repository.record.CommitState(), len(harness.repository.stores), durable.Ranges().Ranges())
	}
	if !bytes.Equal(transaction.Binding().ObjectIdentity().Bytes(), harness.object.Bytes()) {
		t.Fatalf("resumed object = %x, want %x", transaction.Binding().ObjectIdentity().Bytes(), harness.object.Bytes())
	}
}

func TestRecreateInitialCandidateRejectsWrongObjectWithoutAuthority(t *testing.T) {
	harness := newRevisionCandidateRecoveryHarness(t)
	absent, err := NewOwnedObservation(harness.object, OwnedAbsent)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := NewOwnedObservation(harness.object, OwnedReady)
	if err != nil {
		t.Fatal(err)
	}
	wrongObject, err := checkpointmodel.ObjectIDFromBytes(
		bytes.Repeat([]byte{0x7a}, transfer.OwnedObjectIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	wrong := &revisionCandidateOwnedFile{memoryOwnedFile: &memoryOwnedFile{
		object: wrongObject,
		data:   &memoryOwnedData{bytes: make([]byte, harness.candidate.ExactSize())},
	}}
	platform := &revisionCandidatePlatform{Platform: harness.platform}
	platform.open = func(context.Context, checkpointmodel.ObjectID, uint64, bool) (OwnedFile, OwnedObservation, error) {
		return nil, absent, nil
	}
	platform.create = func(context.Context, FileDestination, checkpointmodel.ObjectID, uint64) (OwnedFile, OwnedObservation, error) {
		return wrong, ready, nil
	}
	harness.engine.platform = platform

	start, err := harness.engine.BeginFile(context.Background(), harness.fixture.file, harness.fixture.destination)
	if !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("wrong-object recreation error = %v", err)
	}
	if _, _, hasTransaction := start.Transaction(); hasTransaction {
		t.Fatal("wrong-object observation granted transaction authority")
	}
	if len(harness.repository.stores) != 0 ||
		harness.repository.record.CommitState() != checkpointmodel.CommitCandidate ||
		len(harness.repository.record.VerifiedRanges()) != 0 {
		t.Fatalf("wrong-object observation changed checkpoint authority: stores=%d record=%+v",
			len(harness.repository.stores), harness.repository.record)
	}
	if wrong.syncCalls != 0 || !wrong.closed || !harness.destination.closed {
		t.Fatalf("wrong-object cleanup = (sync=%d fileClosed=%t destinationClosed=%t)",
			wrong.syncCalls, wrong.closed, harness.destination.closed)
	}
}

func TestRecreateInitialCandidateCheckpointLineageProjection(t *testing.T) {
	harness := newRevisionCandidateRecoveryHarness(t)
	spec, err := harness.key.CheckpointLineageSpec()
	if err != nil {
		t.Fatal(err)
	}
	recordSpec, err := harness.candidate.CheckpointLineageSpec()
	if err != nil {
		t.Fatal(err)
	}
	if !checkpointmodel.SameCheckpointLineageSpec(spec, recordSpec) {
		t.Fatalf("key lineage spec = %+v, record spec = %+v", spec, recordSpec)
	}
	if spec.OperationID != harness.key.OperationID() ||
		spec.ReceiveIntentDigest != harness.key.ReceiveIntentDigest() ||
		spec.MaterializationBindingDigest != harness.key.MaterializationBindingDigest() ||
		spec.FileID != harness.key.FileID() || spec.CanonicalPath != harness.key.CanonicalPath() ||
		spec.MaterializerKind != harness.key.MaterializerKind() || spec.AuthorityRef != harness.key.AuthorityRef() {
		t.Fatalf("lineage spec projection = %+v", spec)
	}
	request := harness.key.CheckpointLineageRequest()
	if request.FileRevision != harness.key.FileRevision() || request.ExactSize != harness.key.ExactSize() {
		t.Fatalf("lineage request projection = %+v", request)
	}
	if _, err := (CheckpointKey{}).CheckpointLineageSpec(); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("zero key lineage error = %v", err)
	}
}
