package fileexecution

import (
	"bytes"
	"context"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
)

type collisionRecoveryRepository struct {
	records map[string]checkpointmodel.Record
}

func (repository *collisionRecoveryRepository) Lookup(
	_ context.Context,
	key CheckpointKey,
) (CheckpointResolution, error) {
	record, present := repository.records[key.CanonicalPath()]
	if !present {
		return ResolveCheckpoint(checkpointmodel.CheckpointLineageDecisionAbsent, checkpointmodel.Record{})
	}
	return ResolveCheckpoint(checkpointmodel.CheckpointLineageDecisionExact, record)
}

func (repository *collisionRecoveryRepository) InstallInitial(
	_ context.Context,
	_ CheckpointKey,
	next checkpointmodel.Record,
) (InitialCheckpointObservation, error) {
	path := next.CanonicalPath()
	current, present := repository.records[path]
	if present {
		resolution, _ := ResolveCheckpoint(checkpointmodel.CheckpointLineageDecisionExact, current)
		return ObserveInitialCheckpoint(resolution, false)
	}
	repository.records[path] = next
	resolution, _ := ResolveCheckpoint(checkpointmodel.CheckpointLineageDecisionExact, next)
	return ObserveInitialCheckpoint(resolution, true)
}

func (repository *collisionRecoveryRepository) Replace(
	_ context.Context,
	previous checkpointmodel.Record,
	next checkpointmodel.Record,
) (CheckpointObservation, error) {
	current, present := repository.records[next.CanonicalPath()]
	if !present || !recordEqual(current, previous) {
		if !present {
			return MissingCheckpoint(), nil
		}
		return ObservedCheckpoint(current)
	}
	repository.records[next.CanonicalPath()] = next
	return ObservedCheckpoint(next)
}

type collisionRecoveryDirectories struct {
	destinations map[string]*memoryDestination
}

func (directories *collisionRecoveryDirectories) BindFile(
	_ context.Context,
	_ transfer.MaterializationFile,
	destinationPath transfer.OutputDestinationPath,
) (FileDestination, error) {
	destination := directories.destinations[destinationPath.String()]
	if destination == nil {
		return nil, ErrPortContract
	}
	destination.closed = false
	return destination, nil
}

func TestRecoveredCollisionIsolatesSiblingAndRetriesSameOperation(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	firstPath, err := transfer.NewOutputDestinationPath(fixture.file.ArtifactPath().String())
	if err != nil {
		t.Fatal(err)
	}
	sibling, siblingPath := collisionRecoverySiblingFile(t, fixture, "sibling.bin", 0x61)
	firstDestination := &memoryDestination{
		target: fixture.file.Target(), final: FinalCollision,
	}
	siblingDestination := &memoryDestination{
		target: sibling.Target(), final: FinalAbsent,
	}
	repository := &collisionRecoveryRepository{records: map[string]checkpointmodel.Record{}}
	platform := newMemoryPlatform()
	random := make([]byte, transfer.OwnedObjectIdentityBytes*MaximumObjectAllocationAttempts)
	for block := range MaximumObjectAllocationAttempts {
		for index := range transfer.OwnedObjectIdentityBytes {
			random[block*transfer.OwnedObjectIdentityBytes+index] = byte(block + 1)
		}
	}
	engine, err := New(Config{
		Intent: fixture.intent, Ownership: fixture.ownership, SessionID: fixture.session,
		Directories: &collisionRecoveryDirectories{destinations: map[string]*memoryDestination{
			fixture.file.ArtifactPath().String(): firstDestination,
			sibling.ArtifactPath().String():      siblingDestination,
		}},
		Platform: platform, Checkpoints: repository, Random: bytes.NewReader(random),
	})
	if err != nil {
		t.Fatal(err)
	}

	firstKey, err := engine.checkpointKey(fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	firstObject, err := checkpointmodel.ObjectIDFromBytes(
		bytes.Repeat([]byte{0xf3}, transfer.OwnedObjectIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := newInitialRecord(firstKey, firstObject)
	if err != nil {
		t.Fatal(err)
	}
	active, err := checkpointmodel.PromoteInitialCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	checkpointCandidate, err := checkpointmodel.AdvanceGeneration(
		active, []checkpointmodel.Range{{Offset: 0, End: 2}},
		checkpointmodel.PhaseActive, checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	repository.records[fixture.file.ArtifactPath().String()] = checkpointCandidate
	platform.objects[firstObject] = &memoryOwnedData{bytes: []byte{'d', 'a', 0, 0}}

	firstStart, err := engine.BeginFile(context.Background(), fixture.file, firstPath)
	if err != nil {
		t.Fatal(err)
	}
	firstSettlement, immediate := firstStart.ImmediateSettlement()
	if !immediate || firstSettlement.Kind() != transfer.FileCollision ||
		firstDestination.final != FinalCollision {
		t.Fatalf("first collision = (immediate %t kind %d final %d)",
			immediate, firstSettlement.Kind(), firstDestination.final)
	}
	if recovered := repository.records[fixture.file.ArtifactPath().String()]; recovered.Phase() != checkpointmodel.PhaseActive ||
		recovered.CommitState() != checkpointmodel.CommitVerified ||
		recovered.QuarantineReason() != 0 {
		t.Fatalf("collision record = (phase %d commit %d reason %d)",
			recovered.Phase(), recovered.CommitState(), recovered.QuarantineReason())
	}

	// A file-local collision must not consume session or checkpoint authority
	// needed by an independent sibling in the same receive operation.
	siblingStart, err := engine.BeginFile(context.Background(), sibling, siblingPath)
	if err != nil {
		t.Fatal(err)
	}
	siblingTransaction, _, ok := siblingStart.Transaction()
	if !ok {
		t.Fatal("collision stopped sibling transaction admission")
	}
	if err := siblingTransaction.WriteRange(context.Background(), 0, []byte("peer")); err != nil {
		t.Fatal(err)
	}
	siblingSettlement, err := siblingTransaction.Commit(context.Background())
	if err != nil || siblingSettlement.Kind() != transfer.FilePublished {
		t.Fatalf("sibling commit = (%d, %v)", siblingSettlement.Kind(), err)
	}

	firstDestination.final = FinalAbsent
	retryStart, err := engine.BeginFile(context.Background(), fixture.file, firstPath)
	if err != nil {
		t.Fatal(err)
	}
	retryTransaction, durable, ok := retryStart.Transaction()
	if !ok {
		t.Fatal("collision retry did not return a transaction")
	}
	ranges := durable.Ranges().Ranges()
	if len(ranges) != 1 || ranges[0] != (content.Range{Offset: 0, End: 2}) {
		t.Fatalf("collision retry durable ranges = %+v", ranges)
	}
	if !bytes.Equal(retryTransaction.Binding().ObjectIdentity().Bytes(), firstObject.Bytes()) {
		t.Fatalf("collision retry did not retain owned object: transaction=%t object=%x want=%x",
			ok, retryTransaction.Binding().ObjectIdentity().Bytes(), firstObject.Bytes())
	}
	if err := retryTransaction.WriteRange(context.Background(), 2, []byte("ta")); err != nil {
		t.Fatal(err)
	}
	retrySettlement, err := retryTransaction.Commit(context.Background())
	if err != nil || retrySettlement.Kind() != transfer.FilePublished {
		t.Fatalf("collision retry commit = (%d, %v)", retrySettlement.Kind(), err)
	}
	if firstDestination.final != FinalOwnedExact || siblingDestination.final != FinalOwnedExact {
		t.Fatalf("final observations = first %d sibling %d",
			firstDestination.final, siblingDestination.final)
	}
}

func collisionRecoverySiblingFile(
	t *testing.T,
	fixture executionFixture,
	path string,
	seed byte,
) (transfer.MaterializationFile, transfer.OutputDestinationPath) {
	t.Helper()
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath(path)
	if err != nil {
		t.Fatal(err)
	}
	fileID, revision := catalog.FileID{}, content.FileRevision{}
	fileID[0], revision[0] = seed, seed+1
	geometry, err := content.NewFileGeometry(4, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		fixture.intent.ShareInstance(), fileID, revision, geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	materializationPath, err := transfer.NewMaterializationRootRelativePath(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := transfer.NewMaterializationFile(
		fixture.intent, sourcePath, materializationPath, descriptor, fixture.session, fixture.file.Parent(),
	)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := transfer.NewOutputDestinationPath(path)
	if err != nil {
		t.Fatal(err)
	}
	return file, destination
}
