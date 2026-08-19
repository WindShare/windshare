package checkpointstore

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/transfer"
)

func TestFileExecutionAuthorityClassifiesPhysicalLineageEvidence(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0xb1)
	defer registry.Close()
	defer lease.Close()
	defer repository.Close()
	base := checkpointRecordFixture(t, ownership, operation, 0xb2)

	revision := base.FileRevision()
	revision[0]++
	revisionConflict := checkpointRecordVariant(t, base, recordVariant{revision: &revision})
	invalidSize := base.ExactSize() + 1
	invalid := checkpointRecordVariant(t, base, recordVariant{exactSize: &invalidSize})
	otherObject := objectIDFixture(t, 0xb8)
	ownershipConflict := checkpointRecordVariant(t, base, recordVariant{object: &otherObject})
	otherPath := "folder/sibling.bin"
	crossLineage := checkpointRecordVariant(t, base, recordVariant{path: &otherPath})

	for name, test := range map[string]struct {
		records []checkpointmodel.Record
		request checkpointmodel.CheckpointLineageRequest
		want    checkpointmodel.CheckpointLineageDecision
	}{
		"absent": {nil, checkpointmodel.CheckpointLineageRequest{
			FileRevision: base.FileRevision(), ExactSize: base.ExactSize(),
		}, checkpointmodel.CheckpointLineageDecisionAbsent},
		"exact": {[]checkpointmodel.Record{base}, checkpointmodel.CheckpointLineageRequest{
			FileRevision: base.FileRevision(), ExactSize: base.ExactSize(),
		}, checkpointmodel.CheckpointLineageDecisionExact},
		"revision conflict": {[]checkpointmodel.Record{base, revisionConflict}, checkpointmodel.CheckpointLineageRequest{
			FileRevision: base.FileRevision(), ExactSize: base.ExactSize(),
		}, checkpointmodel.CheckpointLineageDecisionRevisionConflict},
		"ownership conflict": {[]checkpointmodel.Record{base, ownershipConflict}, checkpointmodel.CheckpointLineageRequest{
			FileRevision: base.FileRevision(), ExactSize: base.ExactSize(),
		}, checkpointmodel.CheckpointLineageDecisionOwnershipConflict},
		"invalid size": {[]checkpointmodel.Record{base, invalid}, checkpointmodel.CheckpointLineageRequest{
			FileRevision: base.FileRevision(), ExactSize: base.ExactSize(),
		}, checkpointmodel.CheckpointLineageDecisionInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			authority := newFileExecutionAuthority()
			if err := authority.rebuild(test.records, nil); err != nil {
				t.Fatal(err)
			}
			spec, err := base.CheckpointLineageSpec()
			if err != nil {
				t.Fatal(err)
			}
			decision, selected, err := authority.classify(spec, test.request)
			if err != nil || decision != test.want {
				t.Fatalf("decision = (%s, %v), want %s", decision, err, test.want)
			}
			if (decision == checkpointmodel.CheckpointLineageDecisionExact) != selected.Valid() {
				t.Fatalf("selected record validity = %t for %s", selected.Valid(), decision)
			}
		})
	}

	authority := newFileExecutionAuthority()
	if err := authority.rebuild([]checkpointmodel.Record{base, crossLineage}, nil); err != nil {
		t.Fatal(err)
	}
	for _, record := range []checkpointmodel.Record{base, crossLineage} {
		spec, _ := record.CheckpointLineageSpec()
		decision, _, err := authority.classify(spec, checkpointmodel.CheckpointLineageRequest{
			FileRevision: record.FileRevision(), ExactSize: record.ExactSize(),
		})
		if err != nil || decision != checkpointmodel.CheckpointLineageDecisionOwnershipConflict {
			t.Fatalf("cross-lineage decision = (%s, %v)", decision, err)
		}
	}
}

func TestConcurrentInitialInstallSelectsOnePhysicalAuthority(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0xc1)
	defer registry.Close()
	defer lease.Close()
	defer repository.Close()
	first, err := NewFreshFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFreshFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	left := checkpointRecordFixture(t, ownership, operation, 0xc2)
	rightObject := objectIDFixture(t, 0xc8)
	right := checkpointRecordVariant(t, left, recordVariant{object: &rightObject})

	type result struct {
		observationInstalled bool
		selected             checkpointmodel.Record
		err                  error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(1)
	install := func(store *FileExecutionStore, candidate checkpointmodel.Record) {
		start.Wait()
		observation, err := store.installInitialRecord(context.Background(), candidate)
		selected, _ := observation.Resolution().Record()
		results <- result{observation.Installed(), selected, err}
	}
	go install(first, left)
	go install(second, right)
	start.Done()

	installed := 0
	var selected checkpointmodel.RecordID
	for range 2 {
		result := <-results
		if result.err != nil || !result.selected.Valid() {
			t.Fatalf("install result = (%+v, %v)", result.selected, result.err)
		}
		if result.observationInstalled {
			installed++
		}
		if selected.IsZero() {
			selected = result.selected.RecordID()
		} else if selected != result.selected.RecordID() {
			t.Fatalf("workers selected different RecordIDs: %x != %x", selected, result.selected.RecordID())
		}
	}
	if installed != 1 || first.RecordCount() != 1 || second.RecordCount() != 1 {
		t.Fatalf("installed=%d counts=(%d,%d)", installed, first.RecordCount(), second.RecordCount())
	}
}

func TestInitialAdmissionRejectsProspectiveClaimsBeforePersistence(t *testing.T) {
	t.Run("object already claimed by another lineage", func(t *testing.T) {
		_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0xc9)
		defer registry.Close()
		defer lease.Close()
		defer repository.Close()
		store, err := NewFreshFileExecutionStore(&repository)
		if err != nil {
			t.Fatal(err)
		}
		first := checkpointRecordFixture(t, ownership, operation, 0xca)
		if observation, err := store.installInitialRecord(context.Background(), first); err != nil || !observation.Installed() {
			t.Fatalf("first install = (%t, %v)", observation.Installed(), err)
		}
		otherPath := "folder/prospective-object.bin"
		prospective := checkpointRecordVariant(t, first, recordVariant{path: &otherPath})
		observation, err := store.installInitialRecord(context.Background(), prospective)
		if !errors.Is(err, fileexecution.ErrCheckpointObjectClaimed) || observation.Installed() {
			t.Fatalf("prospective object admission = (%t, %v)", observation.Installed(), err)
		}
		if _, err := repository.Reopen(prospective.RecordID()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("rejected object claim persisted: %v", err)
		}
		if store.RecordCount() != 1 {
			t.Fatalf("record count after object rejection = %d", store.RecordCount())
		}
		slots, _ := store.LineageSnapshot()
		if len(slots) != 1 || slots[0].Decision() != checkpointmodel.CheckpointLineageDecisionExact {
			t.Fatalf("object rejection poisoned lineage inventory: %+v", slots)
		}
	})

	t.Run("operation record capacity", func(t *testing.T) {
		_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0xcb)
		defer registry.Close()
		defer lease.Close()
		defer repository.Close()
		repository.authority.recordLimit = 1
		store, err := NewFreshFileExecutionStore(&repository)
		if err != nil {
			t.Fatal(err)
		}
		first := checkpointRecordFixture(t, ownership, operation, 0xcc)
		if observation, err := store.installInitialRecord(context.Background(), first); err != nil || !observation.Installed() {
			t.Fatalf("first install = (%t, %v)", observation.Installed(), err)
		}
		otherPath := "folder/over-capacity.bin"
		otherObject := objectIDFixture(t, 0xcd)
		prospective := checkpointRecordVariant(t, first, recordVariant{path: &otherPath, object: &otherObject})
		observation, err := store.installInitialRecord(context.Background(), prospective)
		if !errors.Is(err, fileexecution.ErrCheckpointRecordCapacity) || observation.Installed() {
			t.Fatalf("capacity admission = (%t, %v)", observation.Installed(), err)
		}
		if _, err := repository.Reopen(prospective.RecordID()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("over-capacity checkpoint persisted: %v", err)
		}
		if store.RecordCount() != 1 {
			t.Fatalf("record count after capacity rejection = %d", store.RecordCount())
		}
	})
}

func TestInitialCandidateWithoutObjectRemainsExactAcrossReconciliation(t *testing.T) {
	root, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0xd1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	store, err := NewFreshFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	candidate := checkpointRecordFixture(t, ownership, operation, 0xd2)
	installed, err := store.installInitialRecord(context.Background(), candidate)
	if err != nil || !installed.Installed() {
		t.Fatalf("initial install = (%t, %v)", installed.Installed(), err)
	}

	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	registry, err = OpenOperationRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	lease, err = registry.AcquireOperationLease(operation.intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	binding, err := checkpointmodel.NewBinding(
		ownership, operation.intent.OperationID(), operation.intent.Digest(), operation.intent.BindingDigest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	repository, err = OpenOrdinaryFileRepository(lease, binding, false)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	slots, attention := restarted.LineageSnapshot()
	selected, exact := slots[0].Record()
	if len(slots) != 1 || len(attention) != 0 || !exact ||
		selected.RecordID() != candidate.RecordID() || selected.CommitState() != checkpointmodel.CommitCandidate {
		t.Fatalf("restart candidate = (slots=%d attention=%d exact=%t record=%+v)",
			len(slots), len(attention), exact, selected)
	}
}

func TestRevisionConflictNeverMovesPhysicalVerifiedRanges(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0xe1)
	defer registry.Close()
	defer lease.Close()
	defer repository.Close()
	base := checkpointRecordFixture(t, ownership, operation, 0xe2)
	verified, err := checkpointmodel.PromoteInitialCandidate(base)
	if err != nil {
		t.Fatal(err)
	}
	firstCandidate, err := checkpointmodel.AdvanceGeneration(
		verified, []checkpointmodel.Range{{Offset: 0, End: 8}},
		checkpointmodel.PhaseActive, checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := checkpointmodel.Promote(
		firstCandidate, checkpointmodel.PhaseActive, checkpointmodel.CommitVerified,
	)
	if err != nil {
		t.Fatal(err)
	}
	revision := first.FileRevision()
	revision[0]++
	secondRanges := []checkpointmodel.Range{{Offset: 16, End: 24}}
	second := checkpointRecordVariant(t, first, recordVariant{
		revision: &revision, ranges: &secondRanges,
	})
	authority := newFileExecutionAuthority()
	if err := authority.rebuild([]checkpointmodel.Record{first, second}, nil); err != nil {
		t.Fatal(err)
	}
	slots := authority.lineageSnapshot()
	if len(slots) != 1 || slots[0].Decision() != checkpointmodel.CheckpointLineageDecisionRevisionConflict {
		t.Fatalf("revision slot = %+v", slots)
	}
	physical := slots[0].PhysicalRecords()
	want := map[checkpointmodel.RecordID][]checkpointmodel.Range{
		first.RecordID(): first.VerifiedRanges(), second.RecordID(): second.VerifiedRanges(),
	}
	for _, record := range physical {
		if !slices.Equal(record.VerifiedRanges(), want[record.RecordID()]) {
			t.Fatalf("record %x ranges moved: got=%v want=%v",
				record.RecordID(), record.VerifiedRanges(), want[record.RecordID()])
		}
	}
}

type recordVariant struct {
	revision  *content.FileRevision
	exactSize *uint64
	object    *checkpointmodel.ObjectID
	path      *string
	ranges    *[]checkpointmodel.Range
}

func checkpointRecordVariant(
	t *testing.T,
	record checkpointmodel.Record,
	variant recordVariant,
) checkpointmodel.Record {
	t.Helper()
	revision, exactSize, object, path := record.FileRevision(), record.ExactSize(), record.OwnedObjectID(), record.CanonicalPath()
	ranges := record.VerifiedRanges()
	if variant.revision != nil {
		revision = *variant.revision
	}
	if variant.exactSize != nil {
		exactSize = *variant.exactSize
	}
	if variant.object != nil {
		object = *variant.object
	}
	if variant.path != nil {
		path = *variant.path
	}
	if variant.ranges != nil {
		ranges = slices.Clone(*variant.ranges)
	}
	next, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		OperationID: record.OperationID(), ReceiveIntentDigest: record.ReceiveIntentDigest(),
		MaterializationBindingDigest: record.MaterializationBindingDigest(),
		FileID:                       record.FileID(), FileRevision: revision, CanonicalPath: path,
		ExactSize: exactSize, MaterializerKind: record.MaterializerKind(),
		AuthorityRef: record.AuthorityRef().Bytes(), OwnedObjectID: object.Bytes(),
		StateGeneration: record.StateGeneration(), CheckpointGeneration: record.CheckpointGeneration(),
		VerifiedRanges: ranges, Phase: record.Phase(), CommitState: record.CommitState(),
		QuarantineReason: record.QuarantineReason(), QuarantineOrigin: record.QuarantineOrigin(),
		RetirementReason: record.RetirementReason(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func objectIDFixture(t *testing.T, fill byte) checkpointmodel.ObjectID {
	t.Helper()
	object, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{fill}, transfer.OwnedObjectIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	return object
}
