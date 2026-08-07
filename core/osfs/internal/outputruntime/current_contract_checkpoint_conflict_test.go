package outputruntime

import (
	"context"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestCurrentCheckpointLoaderRejectsDuplicatePathsAndOutputObjects(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	session, intent := currentCoverageSession(t, rootSpec, 0x60, 0x61)
	parent := currentCoverageRoot(t, session, intent, 0x62)
	file := currentCoverageFile(t, session, intent, "original.bin", parent, 0x63, 0x64, 1)
	_ = currentCoverageTransaction(t, session, file)
	t.Cleanup(func() { _, _ = session.PauseJob(context.Background(), transfer.JobPauseInterrupted) })
	original := session.inner.incrementalCheckpointByPath[file.Path]
	if original.RecordID().IsZero() {
		t.Fatal("live transaction did not commit its initial FileCheckpointV1")
	}

	otherObject := original.OwnedOutputObject().Bytes()
	otherObject[0] ^= 0xff
	duplicatePath := currentCheckpointVariant(t, original, currentCheckpointVariantSpec{
		fileID:     incrementalTestIdentity16[catalog.FileID](0x65),
		revision:   incrementalTestIdentity16[content.FileRevision](0x66),
		object:     otherObject,
		path:       original.CanonicalPath(),
		generation: original.StateGeneration() + 1,
	})
	pathLoader := newCurrentCheckpointLoader()
	pathLoader.admitCheckpoint(original, "a/original")
	pathLoader.admitCheckpoint(duplicatePath, "b/duplicate-path")
	if _, found := pathLoader.byPath[original.CanonicalPath()]; found {
		t.Fatal("duplicate path retained one arbitrary checkpoint")
	}
	if _, blocked := pathLoader.blockedPaths[original.CanonicalPath()]; !blocked || len(pathLoader.attention) != 1 {
		t.Fatalf("duplicate path evidence = blocked:%t attention:%d", blocked, len(pathLoader.attention))
	}
	// Once ambiguity is established, later directory-order variants cannot win.
	pathLoader.admitCheckpoint(original, "c/replayed-original")
	if _, found := pathLoader.byPath[original.CanonicalPath()]; found {
		t.Fatal("blocked path was re-admitted")
	}

	duplicateObject := currentCheckpointVariant(t, original, currentCheckpointVariantSpec{
		fileID:     incrementalTestIdentity16[catalog.FileID](0x67),
		revision:   incrementalTestIdentity16[content.FileRevision](0x68),
		object:     original.OwnedOutputObject().Bytes(),
		path:       "other.bin",
		generation: original.StateGeneration() + 1,
	})
	objectLoader := newCurrentCheckpointLoader()
	objectLoader.admitCheckpoint(original, "a/original")
	objectLoader.admitCheckpoint(duplicateObject, "b/duplicate-object")
	if len(objectLoader.byPath) != 0 || len(objectLoader.byKey) != 0 || len(objectLoader.byObject) != 0 {
		t.Fatalf("duplicate object retained claims: paths=%d keys=%d objects=%d",
			len(objectLoader.byPath), len(objectLoader.byKey), len(objectLoader.byObject))
	}
	if _, blocked := objectLoader.blockedObjects[original.OwnedOutputObject()]; !blocked || len(objectLoader.attention) != 2 {
		t.Fatalf("duplicate object evidence = blocked:%t attention:%d", blocked, len(objectLoader.attention))
	}
	objectLoader.admitCheckpoint(duplicateObject, "c/replayed-duplicate")
	if len(objectLoader.byPath) != 0 {
		t.Fatal("blocked object was re-admitted")
	}

	if !checkpointByPathMatches(
		map[string]resumestate.FileCheckpointV1{original.CanonicalPath(): original},
		original.CanonicalPath(), original,
	) {
		t.Fatal("exact path checkpoint did not match")
	}
	if checkpointByPathMatches(nil, original.CanonicalPath(), original) || checkpointByPathMatches(
		map[string]resumestate.FileCheckpointV1{original.CanonicalPath(): original},
		original.CanonicalPath(), duplicatePath,
	) {
		t.Fatal("checkpoint path match ignored immutable identity")
	}
}

func TestCurrentCheckpointClaimsInstallBeforeNewObjectAllocation(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	session, intent := currentCoverageSession(t, rootSpec, 0x69, 0x6a)
	parent := currentCoverageRoot(t, session, intent, 0x6b)
	file := currentCoverageFile(t, session, intent, "claim.bin", parent, 0x6c, 0x6d, 1)
	_ = currentCoverageTransaction(t, session, file)
	t.Cleanup(func() { _, _ = session.PauseJob(context.Background(), transfer.JobPauseInterrupted) })
	checkpoint := session.inner.incrementalCheckpointByPath[file.Path]
	objectID, err := resumestate.OutputObjectIDFromBytes(checkpoint.OwnedOutputObject().Bytes())
	if err != nil {
		t.Fatal(err)
	}

	fresh := &Session{}
	if err := fresh.installCheckpointObjectClaims(map[string]resumestate.FileCheckpointV1{
		file.Path: checkpoint,
	}); err != nil {
		t.Fatal(err)
	}
	if got := fresh.objectClaims[objectID]; got != resumestate.DigestCanonicalLocator(file.Path) {
		t.Fatalf("restored object claim = %x, want path digest", got)
	}
	fresh.objectClaims[objectID] = resumestate.DigestCanonicalLocator("foreign.bin")
	if err := fresh.installCheckpointObjectClaims(map[string]resumestate.FileCheckpointV1{
		file.Path: checkpoint,
	}); err == nil {
		t.Fatal("foreign restored object claim was overwritten")
	}

	loader := persistedCheckpointLoader{
		session:      session.inner,
		rootIdentity: checkpoint.RootIdentity(),
	}
	if !loader.validBinding(checkpoint, checkpoint.RecordID()) {
		t.Fatal("valid checkpoint binding was rejected")
	}
	if loader.validBinding(checkpoint, resumestate.FileCheckpointRecordID{}) {
		t.Fatal("checkpoint accepted a foreign record name")
	}
	if key := liveFileKeyForCheckpoint(checkpoint); key.CanonicalPath != file.Path || key.FileID != checkpoint.FileID() {
		t.Fatalf("live key lost immutable binding: %+v", key)
	}
	if !committedCheckpointState(resumestate.FileCheckpointCommitVerified) ||
		!committedCheckpointState(resumestate.FileCheckpointCommitPublished) ||
		!committedCheckpointState(resumestate.FileCheckpointCommitQuarantined) ||
		committedCheckpointState(resumestate.FileCheckpointCommitCandidate) {
		t.Fatal("checkpoint loader commit-state filter changed")
	}
	invalidShardLoader := newCurrentCheckpointLoader()
	if err := invalidShardLoader.loadShards([]string{"unsafe"}); err != nil || len(invalidShardLoader.attention) != 1 {
		t.Fatalf("invalid shard classification = attention:%d err:%v", len(invalidShardLoader.attention), err)
	}
}

type currentCheckpointVariantSpec struct {
	fileID     catalog.FileID
	revision   content.FileRevision
	object     []byte
	path       string
	generation uint64
}

func currentCheckpointVariant(
	t *testing.T,
	checkpoint resumestate.FileCheckpointV1,
	variant currentCheckpointVariantSpec,
) resumestate.FileCheckpointV1 {
	t.Helper()
	created, err := resumestate.NewFileCheckpointV1(resumestate.FileCheckpointSpec{
		OwnershipMarker: checkpoint.OwnershipMarker(), Namespace: checkpoint.Namespace(),
		TransferIntentDigest: checkpoint.TransferIntentDigest(), FileID: variant.fileID,
		FileRevision: variant.revision, CanonicalPath: variant.path, ExactSize: checkpoint.ExactSize(),
		BackendID: string(checkpoint.BackendID()), RootIdentity: checkpoint.RootIdentity().Bytes(),
		OwnedOutputObject: variant.object, StateGeneration: variant.generation,
		CheckpointGeneration: checkpoint.CheckpointGeneration(), VerifiedRanges: checkpoint.VerifiedRanges(),
		Phase: checkpoint.Phase(), CommitState: checkpoint.CommitState(),
		QuarantineReason: checkpoint.QuarantineReason(), PhaseBeforeQuarantine: checkpoint.PhaseBeforeQuarantine(),
		RetirementReason: checkpoint.RetirementReason(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func newCurrentCheckpointLoader() *persistedCheckpointLoader {
	return &persistedCheckpointLoader{
		byPath:         make(map[string]resumestate.FileCheckpointV1),
		byKey:          make(map[resumestate.LiveFileKey]resumestate.FileCheckpointV1),
		byObject:       make(map[resumestate.FileCheckpointObjectID]persistedCheckpointRecord),
		blockedPaths:   make(map[string]struct{}),
		blockedObjects: make(map[resumestate.FileCheckpointObjectID]struct{}),
	}
}
