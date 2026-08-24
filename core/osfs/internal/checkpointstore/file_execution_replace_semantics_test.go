package checkpointstore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
)

const (
	replaceExactSize      = 64
	replaceRevisionFill   = 0xf1
	replaceSessionFill    = 0xf2
	replaceGenerationFill = 0xf4
)

var errCheckpointKeyCaptured = errors.New("checkpoint key captured")

func TestFileExecutionStoreReplaceDurablyAdvancesExactLineage(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0xe8)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})

	file, session, destination := replacementMaterializationFixture(t, operation.intent)
	object := objectIDFixture(t, 0xe9)
	initial := replacementRecordFixture(t, operation.intent, ownership, file, object)
	previous, err := checkpointmodel.PromoteInitialCandidate(initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(initial); err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(initial, previous); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	next, err := checkpointmodel.AdvanceGeneration(
		previous,
		[]checkpointmodel.Range{{Offset: 0, End: 8}},
		checkpointmodel.PhaseActive,
		checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}

	observation, err := store.Replace(context.Background(), previous, next)
	observed, present := observation.Record()
	if err != nil || !present || !bytes.Equal(observed.CanonicalBytes(), next.CanonicalBytes()) {
		t.Fatalf("Replace() = (present=%t, record=%+v, err=%v), want exact next generation", present, observed, err)
	}
	durable, err := repository.Reopen(next.RecordID())
	if err != nil || !bytes.Equal(durable.CanonicalBytes(), next.CanonicalBytes()) {
		t.Fatalf("durable replacement = (%+v, %v), want next generation", durable, err)
	}

	key := captureCheckpointKey(t, operation.intent, ownership, session, file, destination, store)
	resolution, err := store.Lookup(context.Background(), key)
	selected, exact := resolution.Record()
	if err != nil || resolution.Decision() != checkpointmodel.CheckpointLineageDecisionExact ||
		!exact || !bytes.Equal(selected.CanonicalBytes(), next.CanonicalBytes()) {
		t.Fatalf("Lookup() = (decision=%s, exact=%t, record=%+v, err=%v), want exact next generation",
			resolution.Decision(), exact, selected, err)
	}

	slots, attention := store.LineageSnapshot()
	if len(slots) != 1 || len(attention) != 0 {
		t.Fatalf("LineageSnapshot() sizes = (%d, %d), want (1, 0)", len(slots), len(attention))
	}
	selected, exact = slots[0].Record()
	physical := slots[0].PhysicalRecords()
	if slots[0].Decision() != checkpointmodel.CheckpointLineageDecisionExact ||
		slots[0].CanonicalPath() != next.CanonicalPath() || !exact ||
		!bytes.Equal(selected.CanonicalBytes(), next.CanonicalBytes()) || len(physical) != 1 ||
		!bytes.Equal(physical[0].CanonicalBytes(), next.CanonicalBytes()) ||
		bytes.Equal(physical[0].CanonicalBytes(), previous.CanonicalBytes()) {
		t.Fatalf("LineageSnapshot() retained stale authority: decision=%s path=%q exact=%t selected=%+v physical=%+v",
			slots[0].Decision(), slots[0].CanonicalPath(), exact, selected, physical)
	}
}

func TestFileExecutionAuthorityReplaceRejectsMissingOrChangedBinding(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0xea)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	initial := checkpointRecordFixture(t, ownership, operation, 0xeb)
	previous, err := checkpointmodel.PromoteInitialCandidate(initial)
	if err != nil {
		t.Fatal(err)
	}
	next, err := checkpointmodel.AdvanceGeneration(
		previous,
		[]checkpointmodel.Range{{Offset: 0, End: 8}},
		checkpointmodel.PhaseActive,
		checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("missing previous", func(t *testing.T) {
		authority := newFileExecutionAuthority()
		if err := authority.replace(previous, next); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
			t.Fatalf("replace missing previous error = %v, want %v", err, checkpointmodel.ErrRecordBinding)
		}
		if len(authority.physical) != 0 || len(authority.lineages) != 0 || len(authority.objects) != 0 {
			t.Fatalf("missing replacement mutated authority: physical=%d lineages=%d objects=%d",
				len(authority.physical), len(authority.lineages), len(authority.objects))
		}
	})

	otherPath := "folder/changed-lineage.bin"
	otherObject := objectIDFixture(t, 0xec)
	for name, changed := range map[string]checkpointmodel.Record{
		"lineage": checkpointRecordVariant(t, next, recordVariant{path: &otherPath}),
		"object":  checkpointRecordVariant(t, next, recordVariant{object: &otherObject}),
	} {
		t.Run(name, func(t *testing.T) {
			authority := newFileExecutionAuthority()
			if err := authority.rebuild([]checkpointmodel.Record{previous}, nil); err != nil {
				t.Fatal(err)
			}
			if err := authority.replace(previous, changed); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
				t.Fatalf("replace changed %s binding error = %v, want %v", name, err, checkpointmodel.ErrRecordBinding)
			}
			current := authority.physical[previous.RecordID()]
			slots := authority.lineageSnapshot()
			if len(slots) != 1 {
				t.Fatalf("rejected %s binding changed lineage count: %+v", name, slots)
			}
			selected, exact := slots[0].Record()
			if len(authority.physical) != 1 || !bytes.Equal(current.CanonicalBytes(), previous.CanonicalBytes()) ||
				len(authority.objects) != 1 || len(authority.objects[previous.OwnedObjectID()]) != 1 ||
				!exact || !bytes.Equal(selected.CanonicalBytes(), previous.CanonicalBytes()) {
				t.Fatalf("rejected %s binding changed authority: physical=%+v objects=%+v slots=%+v",
					name, authority.physical, authority.objects, slots)
			}
		})
	}
}

func replacementMaterializationFixture(
	t *testing.T,
	intent transfer.ReceiveIntent,
) (transfer.MaterializationFile, transfer.OutputSessionID, transfer.OutputDestinationPath) {
	t.Helper()
	layout, ok := intent.ArtifactSpec().DirectoryTree()
	if !ok {
		t.Fatal("replacement fixture intent is not a directory tree")
	}
	original, ok := layout.SingleFile()
	if !ok {
		t.Fatal("replacement fixture intent is not a single-file tree")
	}
	var revision content.FileRevision
	var generation catalog.DirectoryGeneration
	for index := range revision {
		revision[index] = replaceRevisionFill
		generation[index] = replaceGenerationFill
	}
	geometry, err := content.NewFileGeometry(replaceExactSize, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(), original.FileID, revision, geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath(original.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	session, err := transfer.OutputSessionIDFromBytes(
		bytes.Repeat([]byte{replaceSessionFill}, transfer.OutputSessionIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	rootSource := transfer.AuthenticatedSourceDirectory{
		DirectoryID: intent.SyntheticRoot(), Generation: generation,
		SourcePath: ordinaryoutput.EmptySourceCatalogPath(),
	}
	parent, err := transfer.NewReferenceMaterializationFileParent(
		rootSource.DirectoryID, rootSource.Generation, rootSource.SourcePath,
	)
	if err != nil {
		t.Fatal(err)
	}
	materializationPath, err := transfer.NewMaterializationRootRelativePath(original.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := transfer.NewMaterializationFile(
		intent, sourcePath, materializationPath, descriptor, session, parent,
	)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := transfer.NewOutputDestinationPath(file.ArtifactPath().String())
	if err != nil {
		t.Fatal(err)
	}
	return file, session, destination
}

func replacementRecordFixture(
	t *testing.T,
	intent transfer.ReceiveIntent,
	ownership checkpointmodel.Ownership,
	file transfer.MaterializationFile,
	object checkpointmodel.ObjectID,
) checkpointmodel.Record {
	t.Helper()
	descriptor := file.Descriptor()
	record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		OperationID:                  intent.OperationID(),
		ReceiveIntentDigest:          intent.Digest(),
		MaterializationBindingDigest: intent.BindingDigest(),
		FileID:                       descriptor.FileID(),
		FileRevision:                 descriptor.FileRevision(),
		CanonicalPath:                file.ArtifactPath().String(),
		ExactSize:                    file.ExpectedSize(),
		MaterializerKind:             ownership.MaterializerKind(),
		AuthorityRef:                 ownership.AuthorityRef().Bytes(),
		OwnedObjectID:                object.Bytes(),
		StateGeneration:              1,
		CheckpointGeneration:         0,
		Phase:                        checkpointmodel.PhaseActive,
		CommitState:                  checkpointmodel.CommitCandidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func captureCheckpointKey(
	t *testing.T,
	intent transfer.ReceiveIntent,
	ownership checkpointmodel.Ownership,
	session transfer.OutputSessionID,
	file transfer.MaterializationFile,
	destination transfer.OutputDestinationPath,
	platform fileexecution.Platform,
) fileexecution.CheckpointKey {
	t.Helper()
	probe := &checkpointKeyProbe{}
	engine, err := fileexecution.New(fileexecution.Config{
		Intent: intent, Ownership: ownership, SessionID: session,
		Directories: checkpointKeyDirectory{target: file.Target()},
		Platform:    platform, Checkpoints: probe,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.BeginFile(context.Background(), file, destination); !errors.Is(err, errCheckpointKeyCaptured) {
		t.Fatalf("capture checkpoint key error = %v, want %v", err, errCheckpointKeyCaptured)
	}
	if probe.calls != 1 {
		t.Fatalf("checkpoint key probe calls = %d, want 1", probe.calls)
	}
	return probe.key
}

type checkpointKeyProbe struct {
	key   fileexecution.CheckpointKey
	calls int
}

func (probe *checkpointKeyProbe) Lookup(
	_ context.Context,
	key fileexecution.CheckpointKey,
) (fileexecution.CheckpointResolution, error) {
	probe.key = key
	probe.calls++
	return fileexecution.CheckpointResolution{}, errCheckpointKeyCaptured
}

func (*checkpointKeyProbe) InstallInitial(
	context.Context,
	fileexecution.CheckpointKey,
	checkpointmodel.Record,
) (fileexecution.InitialCheckpointObservation, error) {
	return fileexecution.InitialCheckpointObservation{}, errCheckpointKeyCaptured
}

func (*checkpointKeyProbe) Replace(
	context.Context,
	checkpointmodel.Record,
	checkpointmodel.Record,
) (fileexecution.CheckpointObservation, error) {
	return fileexecution.CheckpointObservation{}, errCheckpointKeyCaptured
}

type checkpointKeyDirectory struct {
	target transfer.FileMaterializationTarget
}

func (directory checkpointKeyDirectory) BindFile(
	context.Context,
	transfer.MaterializationFile,
	transfer.OutputDestinationPath,
) (fileexecution.FileDestination, error) {
	return checkpointKeyDestination{target: directory.target}, nil
}

type checkpointKeyDestination struct {
	target transfer.FileMaterializationTarget
}

func (destination checkpointKeyDestination) Target() transfer.FileMaterializationTarget {
	return destination.target
}

func (checkpointKeyDestination) ObserveFinal(
	context.Context,
	fileexecution.FinalExpectation,
) (fileexecution.FinalObservation, error) {
	return fileexecution.FinalObservation{}, errCheckpointKeyCaptured
}

func (checkpointKeyDestination) ObserveFinalPresence(
	context.Context,
) (fileexecution.FinalObservation, error) {
	return fileexecution.FinalObservation{}, errCheckpointKeyCaptured
}

func (checkpointKeyDestination) PublishNoReplace(
	context.Context,
	fileexecution.OwnedFile,
	fileexecution.FinalExpectation,
) (fileexecution.FinalObservation, error) {
	return fileexecution.FinalObservation{}, errCheckpointKeyCaptured
}

func (checkpointKeyDestination) SyncFinalParent(context.Context) error {
	return errCheckpointKeyCaptured
}

func (checkpointKeyDestination) Close() error { return nil }
