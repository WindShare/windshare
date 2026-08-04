package transfer

import (
	"errors"
	"io"
	"math"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestOutputSelectionRejectsCorruptTerminalPlans(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xe1)
	root := transferID[catalog.DirectoryID](0xe2)
	rootGeneration := transferID[catalog.DirectoryGeneration](0xe3)
	directory := selectionPlanRecord{
		kind: selectionPlanDirectoryKind, active: true, path: "folder",
		directory: plannedDirectory{
			directory:  transferID[catalog.DirectoryID](0xe4),
			generation: transferID[catalog.DirectoryGeneration](0xe5),
			path:       "folder",
		},
	}
	nestedFile := selectionPlanRecord{
		kind: selectionPlanFileKind, active: true, path: "folder/nested.bin",
		file: plannedFile{
			file:             transferID[catalog.FileID](0xe6),
			path:             "folder/nested.bin",
			parentDirectory:  directory.directory.directory,
			parentGeneration: directory.directory.generation,
		},
	}
	rootFile := selectionPlanRecord{
		kind: selectionPlanFileKind, active: true, path: "root.bin",
		file: plannedFile{
			file:             transferID[catalog.FileID](0xe7),
			path:             "root.bin",
			parentDirectory:  root,
			parentGeneration: rootGeneration,
		},
	}
	validRecords := []selectionPlanRecord{directory, nestedFile, rootFile}

	if _, err := newOutputSelectionFromPlan(
		catalog.ShareInstance{}, root, rootGeneration,
		&fixedOutputSelectionPlan{},
	); !errors.Is(err, ErrInvalidOutputSelection) {
		t.Fatalf("invalid plan admission = %v", err)
	}
	if _, err := newOutputSelectionFromPlan(share, root, rootGeneration, nil); !errors.Is(err, ErrInvalidOutputSelection) {
		t.Fatalf("nil plan admission = %v", err)
	}
	if _, err := newOutputSelectionFromPlan(share, root, rootGeneration, &fixedOutputSelectionPlan{
		directories: maximumSelectionClaims,
		files:       1,
	}); !errors.Is(err, ErrInvalidOutputSelection) {
		t.Fatalf("oversized plan admission = %v", err)
	}

	visitorFailure := errors.New("terminal plan read failed")
	tests := []struct {
		name string
		plan *fixedOutputSelectionPlan
		want error
	}{
		{
			name: "visit failure",
			plan: &fixedOutputSelectionPlan{visitErr: visitorFailure},
			want: visitorFailure,
		},
		{
			name: "inactive record",
			plan: planWithRecords(withSelectionRecord(validRecords, 0, func(record *selectionPlanRecord) {
				record.active = false
			})),
			want: ErrInvalidOutputSelection,
		},
		{
			name: "out of order",
			plan: planWithRecords([]selectionPlanRecord{rootFile, directory}),
			want: ErrInvalidOutputSelection,
		},
		{
			name: "missing nested parent",
			plan: planWithRecords([]selectionPlanRecord{nestedFile}),
			want: ErrInvalidOutputSelection,
		},
		{
			name: "directory payload mismatch",
			plan: planWithRecords(withSelectionRecord(validRecords, 0, func(record *selectionPlanRecord) {
				record.directory.path = "other"
			})),
			want: ErrInvalidOutputSelection,
		},
		{
			name: "file payload mismatch",
			plan: planWithRecords(withSelectionRecord(validRecords, 1, func(record *selectionPlanRecord) {
				record.file.path = "other"
			})),
			want: ErrInvalidOutputSelection,
		},
		{
			name: "foreign root binding",
			plan: planWithRecords(withSelectionRecord(validRecords, 2, func(record *selectionPlanRecord) {
				record.file.parentDirectory = transferID[catalog.DirectoryID](0xe8)
			})),
			want: ErrInvalidOutputSelection,
		},
		{
			name: "foreign nested binding",
			plan: planWithRecords(withSelectionRecord(validRecords, 1, func(record *selectionPlanRecord) {
				record.file.parentGeneration = transferID[catalog.DirectoryGeneration](0xe9)
			})),
			want: ErrInvalidOutputSelection,
		},
		{
			name: "unknown record kind",
			plan: planWithRecords([]selectionPlanRecord{{kind: 99, active: true, path: "unknown"}}),
			want: ErrInvalidOutputSelection,
		},
		{
			name: "declared count mismatch",
			plan: &fixedOutputSelectionPlan{records: validRecords, directories: 2, files: 2},
			want: ErrInvalidOutputSelection,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newOutputSelectionFromPlan(share, root, rootGeneration, test.plan)
			if !errors.Is(err, test.want) {
				t.Fatalf("corrupt terminal plan error = %v, want %v", err, test.want)
			}
		})
	}

	// Validation and hashing deliberately traverse independently. A storage
	// failure between those passes must abort identity creation rather than bind
	// a partially observed plan.
	changing := planWithRecords(validRecords)
	changing.failCall = 2
	changing.visitErr = visitorFailure
	if _, err := newOutputSelectionFromPlan(share, root, rootGeneration, changing); !errors.Is(err, visitorFailure) {
		t.Fatalf("hash traversal failure = %v", err)
	}
}

func TestOutputSelectionVisitorContractsFailClosed(t *testing.T) {
	var empty OutputSelection
	if empty.DirectoryCount() != 0 || empty.FileCount() != 0 {
		t.Fatalf("zero selection counts = %d/%d", empty.DirectoryCount(), empty.FileCount())
	}
	if err := empty.VisitDirectories(func(OutputSelectionDirectory) error { return nil }); !errors.Is(err, ErrInvalidOutputSelection) {
		t.Fatalf("zero directory visit = %v", err)
	}
	if err := empty.VisitFiles(func(OutputSelectionFile) error { return nil }); !errors.Is(err, ErrInvalidOutputSelection) {
		t.Fatalf("zero file visit = %v", err)
	}
	plan := &memoryOutputSelectionPlan{records: []selectionPlanRecord{{active: true, path: "entry"}}}
	visitorFailure := errors.New("visitor stopped")
	if err := plan.VisitRecords(func(selectionPlanRecord) error { return visitorFailure }); !errors.Is(err, visitorFailure) {
		t.Fatalf("memory plan visitor error = %v", err)
	}
	selection := OutputSelection{plan: plan}
	if err := selection.VisitDirectories(nil); !errors.Is(err, ErrInvalidOutputSelection) {
		t.Fatalf("nil directory visitor = %v", err)
	}
	if err := selection.VisitFiles(nil); !errors.Is(err, ErrInvalidOutputSelection) {
		t.Fatalf("nil file visitor = %v", err)
	}
	if boundedSelectionCapacity(math.MaxUint64) != 0 {
		t.Fatal("unrepresentable materialized selection capacity was accepted")
	}
}

func TestNewOutputSelectionRejectsNodeIdentityReuse(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xd1)
	root := transferID[catalog.DirectoryID](0xd2)
	rootGeneration := transferID[catalog.DirectoryGeneration](0xd3)
	directoryID := transferID[catalog.DirectoryID](0xd4)
	directoryGeneration := transferID[catalog.DirectoryGeneration](0xd5)
	directory := OutputSelectionDirectory{
		Path: "folder", DirectoryID: directoryID, Generation: directoryGeneration,
	}
	reusedFileID := transferID[catalog.FileID](0xd6)
	tests := []struct {
		name        string
		directories []OutputSelectionDirectory
		files       []OutputSelectionFile
	}{
		{
			name: "synthetic root reused by directory",
			directories: []OutputSelectionDirectory{{
				Path: "root-loop", DirectoryID: root, Generation: directoryGeneration,
			}},
		},
		{
			name:        "directory reused by file",
			directories: []OutputSelectionDirectory{directory},
			files: []OutputSelectionFile{{
				Path: "folder/file.bin", FileID: catalog.FileID(directoryID),
				ParentDirectoryID: directoryID, ParentGeneration: directoryGeneration,
			}},
		},
		{
			name: "file reused by sibling",
			files: []OutputSelectionFile{
				{
					Path: "first.bin", FileID: reusedFileID,
					ParentDirectoryID: root, ParentGeneration: rootGeneration,
				},
				{
					Path: "second.bin", FileID: reusedFileID,
					ParentDirectoryID: root, ParentGeneration: rootGeneration,
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewOutputSelection(
				share, root, rootGeneration, test.directories, test.files,
			); !errors.Is(err, ErrInvalidOutputSelection) {
				t.Fatalf("identity-reusing selection error = %v", err)
			}
		})
	}
}

func TestCanonicalSelectionFailsClosedOnPlanTraversalErrors(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xf1)
	root := transferID[catalog.DirectoryID](0xf2)
	generation := transferID[catalog.DirectoryGeneration](0xf3)
	rules, err := NewPathSelectionRules([]string{"file"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	visitorFailure := errors.New("canonical plan traversal failed")
	brokenPlan := &fixedOutputSelectionPlan{visitErr: visitorFailure}
	selection := OutputSelection{
		identity: SelectionIdentity{1}, share: share, root: root,
		rootGeneration: generation, plan: brokenPlan,
	}
	if _, err := NewCanonicalSelectionV1(request, selection); !errors.Is(err, visitorFailure) {
		t.Fatalf("canonical constructor traversal = %v", err)
	}
	if bytes := (CanonicalSelectionV1{}).Bytes(); bytes != nil {
		t.Fatalf("zero canonical selection bytes = %x", bytes)
	}
	brokenCanonical := CanonicalSelectionV1{
		request: []byte{1}, root: generation, plan: brokenPlan,
	}
	func() {
		defer func() {
			if recovered := recover(); !errors.Is(recovered.(error), visitorFailure) {
				t.Fatalf("canonical materialization panic = %v", recovered)
			}
		}()
		_ = brokenCanonical.Bytes()
	}()

	for failedTraversal := 1; failedTraversal <= 2; failedTraversal++ {
		plan := &fixedOutputSelectionPlan{visitErr: visitorFailure, failCall: failedTraversal}
		if err := writeCanonicalSelectionV1(io.Discard, []byte{1}, generation, plan); !errors.Is(err, visitorFailure) {
			t.Fatalf("canonical traversal %d = %v", failedTraversal, err)
		}
	}
}

type fixedOutputSelectionPlan struct {
	records     []selectionPlanRecord
	directories uint64
	files       uint64
	visitErr    error
	failCall    int
	calls       int
}

func (plan *fixedOutputSelectionPlan) DirectoryCount() uint64 { return plan.directories }
func (plan *fixedOutputSelectionPlan) FileCount() uint64      { return plan.files }

func (plan *fixedOutputSelectionPlan) VisitRecords(visit func(selectionPlanRecord) error) error {
	plan.calls++
	if plan.visitErr != nil && (plan.failCall == 0 || plan.calls == plan.failCall) {
		return plan.visitErr
	}
	for _, record := range plan.records {
		if err := visit(record); err != nil {
			return err
		}
	}
	return nil
}

func planWithRecords(records []selectionPlanRecord) *fixedOutputSelectionPlan {
	plan := &fixedOutputSelectionPlan{records: records}
	for _, record := range records {
		switch record.kind {
		case selectionPlanDirectoryKind:
			plan.directories++
		case selectionPlanFileKind:
			plan.files++
		}
	}
	return plan
}

func withSelectionRecord(
	records []selectionPlanRecord,
	index int,
	mutate func(*selectionPlanRecord),
) []selectionPlanRecord {
	copy := append([]selectionPlanRecord(nil), records...)
	mutate(&copy[index])
	return copy
}
