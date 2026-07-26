package resumestate

import (
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

func identity16[T ~[catalog.IdentityBytes]byte](value byte) T {
	var id T
	for index := range id {
		id[index] = value
	}
	return id
}

func identity32[T ~[OutputObjectIDBytes]byte](value byte) T {
	var id T
	for index := range id {
		id[index] = value
	}
	return id
}

func testBackend(t *testing.T) transfer.OutputBackendID {
	t.Helper()
	backend, err := transfer.NewOutputBackendID("native-filesystem-v3")
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func testModifiedTime(t *testing.T) catalog.ModifiedTime {
	t.Helper()
	modified, err := catalog.NewModifiedTime(123, 456_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	return modified
}

func testSelection(t *testing.T, exactSize uint64) transfer.OutputSelection {
	t.Helper()
	share := identity16[catalog.ShareInstance](2)
	root := identity16[catalog.DirectoryID](3)
	rootGeneration := identity16[catalog.DirectoryGeneration](4)
	directory := transfer.OutputSelectionDirectory{
		Path: "folder", DirectoryID: identity16[catalog.DirectoryID](5),
		Generation: identity16[catalog.DirectoryGeneration](6), ModifiedTime: testModifiedTime(t),
	}
	file := transfer.OutputSelectionFile{
		Path: "folder/file.bin", FileID: identity16[catalog.FileID](7),
		ParentDirectoryID: directory.DirectoryID, ParentGeneration: directory.Generation,
		ExpectedSize: exactSize, ModifiedTime: testModifiedTime(t),
	}
	plan, err := transfer.NewOutputSelection(share, root, rootGeneration, []transfer.OutputSelectionDirectory{directory}, []transfer.OutputSelectionFile{file})
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
	bound, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func testControl(t *testing.T) Control {
	t.Helper()
	control, err := NewControl(ControlSpec{
		Backend: testBackend(t), OutputRoot: testRootBinding(t),
		Certification: CertificationLinuxExt4ProcessRestart,
		Durability:    transfer.DurabilityProcessRestart, Generation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return control
}

func testRootBinding(t *testing.T) OutputRootBinding {
	t.Helper()
	return testRootBindingFor(t, CertificationLinuxExt4ProcessRestart, 5)
}

func testRootBindingFor(t *testing.T, certification CertificationID, value byte) OutputRootBinding {
	t.Helper()
	binding, err := NewOutputRootBinding(
		certification, []byte{"volume"[0], value}, []byte{"object"[0], value},
	)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func testAncestryBinding(t *testing.T, root OutputRootBinding, selection transfer.OutputSelection) OutputAncestryBinding {
	t.Helper()
	claims := []OutputAncestryIdentityClaim{{CanonicalPath: "", IdentityClaim: []byte("test-root-identity")}}
	for _, directory := range selection.Directories() {
		claims = append(claims, OutputAncestryIdentityClaim{
			CanonicalPath: directory.Path,
			IdentityClaim: []byte("test-directory-identity:" + directory.Path),
		})
	}
	binding, err := NewOutputAncestryBinding(root, selection.Identity(), claims)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func testHeaderForSelection(t *testing.T, selection transfer.OutputSelection, lifecycle SessionLifecycle) Header {
	return testHeaderForSelectionAndID(t, selection, lifecycle, identity16[transfer.OutputSessionID](1))
}

func testHeaderForSelectionAndID(
	t *testing.T,
	selection transfer.OutputSelection,
	lifecycle SessionLifecycle,
	sessionID transfer.OutputSessionID,
) Header {
	t.Helper()
	root := testRootBinding(t)
	header, err := NewHeader(HeaderSpec{
		Backend: testBackend(t), SessionID: sessionID, Selection: selection,
		OutputRoot: root, OutputAncestry: testAncestryBinding(t, root, selection),
	})
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle != SessionActive {
		header, err = newHeaderFromClaims(headerClaims{
			backend: header.backend, sessionID: header.sessionID, shareInstance: header.shareInstance,
			syntheticRoot: header.syntheticRoot, resumeIntent: header.resumeIntent,
			selectionIdentity:      header.selectionIdentity,
			selectedDirectoryCount: header.selectedDirectoryCount, selectedFileCount: header.selectedFileCount,
			outputRoot: header.outputRoot, outputAncestry: header.outputAncestry,
			lifecycle: lifecycle, stateGeneration: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return header
}

func testHeader(t *testing.T) Header {
	t.Helper()
	return testHeaderForSelection(t, testSelection(t, 10), SessionActive)
}

func testSessionAuthorityForSelection(t *testing.T, selection transfer.OutputSelection, lifecycle SessionLifecycle) SessionAuthority {
	return testSessionAuthorityForSelectionAndID(
		t, selection, lifecycle, identity16[transfer.OutputSessionID](1),
	)
}

func testSessionAuthorityForSelectionAndID(
	t *testing.T,
	selection transfer.OutputSelection,
	lifecycle SessionLifecycle,
	sessionID transfer.OutputSessionID,
) SessionAuthority {
	t.Helper()
	header := testHeaderForSelectionAndID(t, selection, lifecycle, sessionID)
	authority, err := BindSessionAuthority(
		testControl(t), header, selection, ResumeNamespaceName(selection.ResumeIntent()),
		SessionDirectoryName(header.SessionID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func testSessionAuthority(t *testing.T, lifecycle SessionLifecycle) SessionAuthority {
	t.Helper()
	return testSessionAuthorityForSelection(t, testSelection(t, 10), lifecycle)
}

func testRanges(t *testing.T, ranges ...content.Range) content.RangeSet {
	t.Helper()
	set, err := content.NewRangeSet(ranges)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func testDescriptor(t *testing.T, session SessionAuthority, exactSize uint64) content.FileRevisionDescriptor {
	t.Helper()
	geometry, err := content.NewFileGeometry(exactSize, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		session.Header().shareInstance, identity16[catalog.FileID](7), identity16[content.FileRevision](8),
		geometry, testModifiedTime(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func testFileRecord(t *testing.T, phase FilePhase) FileRecord {
	t.Helper()
	selection := testSelection(t, 10)
	return testFileRecordForSelection(t, testSessionAuthorityForSelection(t, selection, SessionActive), phase)
}

func testFileRecordForSelection(t *testing.T, session SessionAuthority, phase FilePhase) FileRecord {
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
		ranges = testRanges(t, content.Range{Offset: 0, End: 10})
		checkpoint = 1
	case FileRetiring:
		ranges = testRanges(t, content.Range{Offset: 0, End: 10})
		checkpoint = 1
		retirement = RetirementPublished
	case FileQuarantined:
		ranges = testRanges(t, content.Range{Offset: 0, End: 5})
		checkpoint = 1
		quarantine = QuarantineStageMismatch
		before = FileWitnessed
	default:
		t.Fatalf("unsupported test phase %v", phase)
	}
	descriptor := testDescriptor(t, session, 10)
	record, err := newFileRecordFromClaims(fileRecordClaims{
		sessionID: session.Header().sessionID, shareInstance: descriptor.ShareInstance(),
		fileID: descriptor.FileID(), revision: descriptor.FileRevision(),
		canonicalLocator: "folder/file.bin", outputObject: identity32[OutputObjectID](9), exactSize: 10,
		chunkSize:       catalog.DefaultChunkSize,
		stateGeneration: 10, checkpointGeneration: checkpoint, durableRanges: ranges, phase: phase,
		quarantineReason: quarantine, phaseBeforeQuarantine: before, retirementReason: retirement,
		expectedMetadata: ExpectedMetadata{ModifiedTime: descriptor.ModifiedTime()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func testBoundFileRecord(t *testing.T, phase FilePhase) BoundFileRecord {
	t.Helper()
	selection := testSelection(t, 10)
	session := testSessionAuthorityForSelection(t, selection, SessionActive)
	record := testFileRecordForSelection(t, session, phase)
	return bindTestFileRecord(t, session, record)
}

func testResumableFile(t *testing.T, phase FilePhase) ResumableFileAuthority {
	t.Helper()
	bound := testBoundFileRecord(t, phase)
	authority, err := BindResumableFile(bound, testDescriptor(t, bound.Session(), 10))
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func bindTestFileRecord(t *testing.T, session SessionAuthority, record FileRecord) BoundFileRecord {
	t.Helper()
	name := FileRecordName(record.LocatorDigest())
	bound, err := BindFileRecord(session, name.Shard(), name.Name(), record)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}
