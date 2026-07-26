package resumestate

import (
	"errors"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

func TestDuplicateOutputObjectDecisionIsSymmetricForEveryPersistedPhase(t *testing.T) {
	for phase := FileReserved; phase <= FileRetiring; phase++ {
		t.Run(phase.String(), func(t *testing.T) {
			left, right := duplicateOutputObjectRecords(
				t, identity16[transfer.OutputSessionID](1), phase, FileReserved,
				identity32[OutputObjectID](9), identity32[OutputObjectID](9),
			)
			decision, err := ReduceDuplicateOutputObject(left, right)
			reversed, reverseErr := ReduceDuplicateOutputObject(right, left)
			if err != nil || reverseErr != nil || decision != reversed ||
				decision.QuarantineReason() != QuarantineOutputObjectDuplicate {
				t.Fatalf("symmetric decisions = %+v, %+v, errors %v, %v", decision, reversed, err, reverseErr)
			}

			for _, current := range []BoundFileRecord{left, right} {
				next, applyErr := ApplyDuplicateOutputObjectDecision(current, decision)
				if applyErr != nil || next.Record().Phase() != FileQuarantined ||
					next.Record().PhaseBeforeQuarantine() != current.Record().Phase() ||
					next.Record().QuarantineReason() != QuarantineOutputObjectDuplicate ||
					next.Record().StateGeneration() != current.Record().StateGeneration()+1 {
					t.Fatalf("quarantined record = %+v, %v", next.Record(), applyErr)
				}
			}
		})
	}
}

func TestDuplicateOutputObjectDecisionCompletesAfterInterruptedPairInstall(t *testing.T) {
	left, right := duplicateOutputObjectRecords(
		t, identity16[transfer.OutputSessionID](1), FileQuarantined, FileReserved,
		identity32[OutputObjectID](9), identity32[OutputObjectID](9),
	)
	decision, err := ReduceDuplicateOutputObject(left, right)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := ApplyDuplicateOutputObjectDecision(left, decision)
	if err != nil || !reflect.DeepEqual(unchanged.Record(), left.Record()) {
		t.Fatalf("already quarantined member = %+v, %v", unchanged.Record(), err)
	}
	quarantined, err := ApplyDuplicateOutputObjectDecision(right, decision)
	if err != nil || quarantined.Record().QuarantineReason() != QuarantineOutputObjectDuplicate {
		t.Fatalf("remaining member = %+v, %v", quarantined.Record(), err)
	}

	// A fresh locked scan binds a fresh decision to the generations it observed;
	// both already-blocked records then remain unchanged.
	retry, err := ReduceDuplicateOutputObject(unchanged, quarantined)
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range []BoundFileRecord{unchanged, quarantined} {
		next, applyErr := ApplyDuplicateOutputObjectDecision(current, retry)
		if applyErr != nil || !reflect.DeepEqual(next.Record(), current.Record()) {
			t.Fatalf("retry member = %+v, %v", next.Record(), applyErr)
		}
	}
}

func TestDuplicateOutputObjectDecisionRejectsNonDuplicateOrStaleAuthority(t *testing.T) {
	left, right := duplicateOutputObjectRecords(
		t, identity16[transfer.OutputSessionID](1), FileReserved, FileReserved,
		identity32[OutputObjectID](9), identity32[OutputObjectID](9),
	)
	if _, err := ReduceDuplicateOutputObject(left, left); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("same-record error = %v", err)
	}
	_, differentObject := duplicateOutputObjectRecords(
		t, identity16[transfer.OutputSessionID](1), FileReserved, FileReserved,
		identity32[OutputObjectID](9), identity32[OutputObjectID](10),
	)
	if _, err := ReduceDuplicateOutputObject(left, differentObject); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("different-object error = %v", err)
	}
	_, foreign := duplicateOutputObjectRecords(
		t, identity16[transfer.OutputSessionID](2), FileReserved, FileReserved,
		identity32[OutputObjectID](9), identity32[OutputObjectID](9),
	)
	if _, err := ReduceDuplicateOutputObject(left, foreign); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("foreign-session error = %v", err)
	}

	decision, err := ReduceDuplicateOutputObject(left, right)
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := ApplyDuplicateOutputObjectDecision(left, decision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyDuplicateOutputObjectDecision(advanced, decision); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale decision error = %v", err)
	}
	if _, err := ApplyDuplicateOutputObjectDecision(left, DuplicateOutputObjectDecision{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero decision error = %v", err)
	}
}

func TestDuplicateOutputObjectQuarantineRoundTripsThroughCanonicalCodec(t *testing.T) {
	left, right := duplicateOutputObjectRecords(
		t, identity16[transfer.OutputSessionID](1), FileReserved, FileReserved,
		identity32[OutputObjectID](9), identity32[OutputObjectID](9),
	)
	decision, err := ReduceDuplicateOutputObject(left, right)
	if err != nil {
		t.Fatal(err)
	}
	quarantined, err := ApplyDuplicateOutputObjectDecision(left, decision)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeFileRecord(quarantined)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeFileRecord(encoded)
	if err != nil || decoded.Phase() != FileQuarantined ||
		decoded.QuarantineReason() != QuarantineOutputObjectDuplicate ||
		decoded.PhaseBeforeQuarantine() != FileReserved {
		t.Fatalf("decoded quarantine = %+v, %v", decoded, err)
	}
	name := FileRecordName(decoded.LocatorDigest())
	if _, err := BindFileRecord(left.Session(), name.Shard(), name.Name(), decoded); err != nil {
		t.Fatalf("rebind decoded duplicate quarantine: %v", err)
	}
}

func duplicateOutputObjectRecords(
	t *testing.T,
	sessionID transfer.OutputSessionID,
	leftPhase FilePhase,
	rightPhase FilePhase,
	leftObject OutputObjectID,
	rightObject OutputObjectID,
) (BoundFileRecord, BoundFileRecord) {
	t.Helper()
	selection, files := duplicateOutputObjectSelection(t)
	session := testSessionAuthorityForSelectionAndID(t, selection, SessionActive, sessionID)
	return duplicateOutputObjectRecord(t, session, files[0], leftPhase, leftObject, 8),
		duplicateOutputObjectRecord(t, session, files[1], rightPhase, rightObject, 10)
}

func duplicateOutputObjectSelection(t *testing.T) (transfer.OutputSelection, []transfer.OutputSelectionFile) {
	t.Helper()
	share := identity16[catalog.ShareInstance](2)
	root := identity16[catalog.DirectoryID](3)
	rootGeneration := identity16[catalog.DirectoryGeneration](4)
	directory := transfer.OutputSelectionDirectory{
		Path: "folder", DirectoryID: identity16[catalog.DirectoryID](5),
		Generation: identity16[catalog.DirectoryGeneration](6), ModifiedTime: testModifiedTime(t),
	}
	files := []transfer.OutputSelectionFile{
		{
			Path: "folder/first.bin", FileID: identity16[catalog.FileID](7),
			ParentDirectoryID: directory.DirectoryID, ParentGeneration: directory.Generation,
			ExpectedSize: 10, ModifiedTime: testModifiedTime(t),
		},
		{
			Path: "folder/second.bin", FileID: identity16[catalog.FileID](9),
			ParentDirectoryID: directory.DirectoryID, ParentGeneration: directory.Generation,
			ExpectedSize: 10, ModifiedTime: testModifiedTime(t),
		},
	}
	plan, err := transfer.NewOutputSelection(
		share, root, rootGeneration, []transfer.OutputSelectionDirectory{directory}, files,
	)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewCanonicalSelectionV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection, files
}

func duplicateOutputObjectRecord(
	t *testing.T,
	session SessionAuthority,
	selected transfer.OutputSelectionFile,
	phase FilePhase,
	object OutputObjectID,
	revisionByte byte,
) BoundFileRecord {
	t.Helper()
	var ranges content.RangeSet
	checkpoint := uint64(0)
	retirement := RetirementReason(0)
	quarantine := QuarantineReason(0)
	before := FilePhase(0)
	switch phase {
	case FileReserved:
		ranges = testRanges(t)
	case FileWitnessed:
		ranges = testRanges(t, content.Range{Offset: 0, End: 5})
		checkpoint = 1
	case FilePublishing, FilePublishBlocked, FilePublished:
		ranges = testRanges(t, content.Range{Offset: 0, End: selected.ExpectedSize})
		checkpoint = 1
	case FileRetiring:
		ranges = testRanges(t, content.Range{Offset: 0, End: selected.ExpectedSize})
		checkpoint = 1
		retirement = RetirementPublished
	case FileQuarantined:
		ranges = testRanges(t)
		quarantine = QuarantineOutputObjectDuplicate
		before = FileReserved
	default:
		t.Fatalf("unsupported duplicate test phase %v", phase)
	}
	record, err := newFileRecordFromClaims(fileRecordClaims{
		sessionID: session.Header().SessionID(), shareInstance: session.Header().ShareInstance(),
		fileID: selected.FileID, revision: identity16[content.FileRevision](revisionByte),
		canonicalLocator: selected.Path, outputObject: object, exactSize: selected.ExpectedSize,
		chunkSize:       catalog.DefaultChunkSize,
		stateGeneration: 10, checkpointGeneration: checkpoint, durableRanges: ranges, phase: phase,
		quarantineReason: quarantine, phaseBeforeQuarantine: before, retirementReason: retirement,
		expectedMetadata: ExpectedMetadata{ModifiedTime: selected.ModifiedTime},
	})
	if err != nil {
		t.Fatal(err)
	}
	return bindTestFileRecord(t, session, record)
}
