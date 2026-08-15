package receivecontract

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func contractID[T ~[16]byte](seed byte) T {
	var value T
	value[0], value[len(value)-1] = seed, seed^0xff
	return value
}

func contractOpaque(seed byte) []byte {
	value := make([]byte, AuthorityRefBytes)
	value[0], value[len(value)-1] = seed, seed^0xff
	return value
}

func contractStable(seed byte) []byte {
	value := make([]byte, StableIdentityBytes)
	value[0], value[len(value)-1] = seed, seed^0xff
	return value
}

func mustIdentity[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func TestIdentityValuesRejectMissingAuthorityAndDefendBytes(t *testing.T) {
	operation := mustIdentity(OperationIDFromBytes(contractStable(1)))
	reservation := mustIdentity(DestinationReservationIDFromBytes(contractStable(2)))
	workspace := mustIdentity(WorkspaceIDFromBytes(contractStable(3)))
	portable := mustIdentity(PortablePlanIDFromBytes(contractStable(4)))
	authority := mustIdentity(AuthorityRefFromBytes(contractOpaque(5)))
	repository := mustIdentity(RepositoryRefFromBytes(contractOpaque(6)))
	artifact := mustIdentity(ArtifactDigestFromBytes(contractOpaque(7)))
	binding := mustIdentity(BindingDigestFromBytes(contractOpaque(8)))
	values := [][]byte{
		operation.Bytes(), reservation.Bytes(), workspace.Bytes(), portable.Bytes(),
		authority.Bytes(), repository.Bytes(), artifact.Bytes(), binding.Bytes(),
	}
	for index, value := range values {
		value[0] ^= 0xff
		if valuesAgain := [][]byte{
			operation.Bytes(), reservation.Bytes(), workspace.Bytes(), portable.Bytes(),
			authority.Bytes(), repository.Bytes(), artifact.Bytes(), binding.Bytes(),
		}; bytes.Equal(value, valuesAgain[index]) {
			t.Fatalf("identity %d exposed mutable bytes", index)
		}
	}
	if operation.IsZero() || reservation.IsZero() || workspace.IsZero() || portable.IsZero() ||
		authority.IsZero() || repository.IsZero() || artifact.IsZero() || binding.IsZero() {
		t.Fatal("valid identity reported zero")
	}
	if !(OperationID{}).IsZero() || !(DestinationReservationID{}).IsZero() ||
		!(WorkspaceID{}).IsZero() || !(PortablePlanID{}).IsZero() ||
		!(AuthorityRef{}).IsZero() || !(RepositoryRef{}).IsZero() ||
		!(ArtifactDigest{}).IsZero() || !(BindingDigest{}).IsZero() {
		t.Fatal("zero identity reported valid")
	}

	invalid := [][]byte{nil, make([]byte, StableIdentityBytes), make([]byte, StableIdentityBytes+1)}
	for _, raw := range invalid {
		if _, err := OperationIDFromBytes(raw); !errors.Is(err, ErrInvalidReceiveContract) {
			t.Fatalf("invalid operation error=%v", err)
		}
	}
	if _, err := AuthorityRefFromBytes(make([]byte, AuthorityRefBytes)); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero authority error=%v", err)
	}
	if _, err := RepositoryRefFromBytes(contractOpaque(8)[:AuthorityRefBytes-1]); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("short repository error=%v", err)
	}
	if _, err := ArtifactDigestFromBytes(nil); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("missing artifact digest error=%v", err)
	}
	if _, err := BindingDigestFromBytes(make([]byte, AuthorityRefBytes)); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero binding digest error=%v", err)
	}
}

func TestArtifactVariantsPreserveOneCanonicalLayout(t *testing.T) {
	file := contractID[catalog.FileID](10)
	directory := contractID[catalog.DirectoryID](11)
	original, err := NewOriginalFileArtifact(file, "docs/report.txt", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	single, err := NewSingleFileDirectoryTree(file, "docs/report.txt", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	complete, err := NewCompleteDirectoryResultRoot(directory, "docs")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := NewDirectorySelectionResultRoot(directory, "docs")
	if err != nil {
		t.Fatal(err)
	}
	synthetic := NewSyntheticSelectionResultRoot()
	resultTree, err := NewResultRootDirectoryTree(selected)
	if err != nil {
		t.Fatal(err)
	}
	catalogTree := NewCatalogRootDirectoryTree()
	archive, err := NewZipArchiveArtifact(complete)
	if err != nil {
		t.Fatal(err)
	}

	if original.Kind() != ArtifactOriginalFile || original.Digest().IsZero() || original.IsZero() ||
		single.Kind() != ArtifactDirectoryTree || resultTree.Kind() != ArtifactDirectoryTree ||
		catalogTree.Kind() != ArtifactDirectoryTree || archive.Kind() != ArtifactZipArchive {
		t.Fatal("artifact discriminant or digest mismatch")
	}
	originalValue, ok := original.OriginalFile()
	if !ok || originalValue.FileID != file || originalValue.SourcePath != "docs/report.txt" ||
		originalValue.SuggestedName != "report.txt" {
		t.Fatalf("original=%+v ok=%v", originalValue, ok)
	}
	if _, ok := original.DirectoryTree(); ok {
		t.Fatal("original exposed directory payload")
	}
	singleLayout, ok := single.DirectoryTree()
	if !ok || singleLayout.Kind() != DirectoryTreeSingleFile {
		t.Fatalf("single layout=%+v ok=%v", singleLayout, ok)
	}
	if value, ok := singleLayout.SingleFile(); !ok || value != originalValue {
		t.Fatalf("single file=%+v ok=%v", value, ok)
	}
	resultLayout, ok := resultTree.DirectoryTree()
	if root, rootOK := resultLayout.ResultRoot(); !ok || !rootOK || root.Class() != ResultRootDirectorySelection ||
		root.AnchorKind() != ResultRootDirectoryAnchor || root.DirectoryID() != directory ||
		root.SourcePath() != "docs" || root.Name() != "docs-selection" {
		t.Fatalf("result root=%+v layoutOK=%v rootOK=%v", root, ok, rootOK)
	}
	if synthetic.Class() != ResultRootSyntheticSelection || synthetic.AnchorKind() != ResultRootSyntheticAnchor ||
		synthetic.Name() != DefaultResultRootName || synthetic.DirectoryID() != (catalog.DirectoryID{}) {
		t.Fatalf("synthetic=%+v", synthetic)
	}
	zip, ok := archive.ZipArchive()
	if !ok || zip.Layout.Name() != "docs" || zip.SuggestedName != "docs.zip" ||
		zip.Encoding != ZipEncodingStore || zip.Completeness != ArtifactCompleteOnly {
		t.Fatalf("zip=%+v ok=%v", zip, ok)
	}
	canonical := archive.CanonicalBytes()
	canonical[0] ^= 0xff
	if bytes.Equal(canonical, archive.CanonicalBytes()) || bytes.Equal(selected.CanonicalBytes(), synthetic.CanonicalBytes()) {
		t.Fatal("canonical artifact bytes were mutable or variants collided")
	}

	longName := strings.Repeat("界", 84)
	protected, err := AppendProtectedSuffix(longName, PartialSelectionSuffix)
	if err != nil || len([]byte(protected)) > MaxResultComponentBytes || !strings.HasSuffix(protected, PartialSelectionSuffix) {
		t.Fatalf("protected=%q bytes=%d err=%v", protected, len([]byte(protected)), err)
	}
	invalidOriginals := []struct{ path, name string }{
		{"docs/report.txt", "other.txt"}, {"docs/../report.txt", "report.txt"},
		{"docs/.wsresume-file", ".wsresume-file"}, {"", ""},
	}
	for _, test := range invalidOriginals {
		if _, err := NewOriginalFileArtifact(file, test.path, test.name); !errors.Is(err, ErrInvalidReceiveContract) {
			t.Fatalf("invalid original %+v error=%v", test, err)
		}
	}
	if _, err := NewSingleFileDirectoryTree(catalog.FileID{}, "report.txt", "report.txt"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero single-file error=%v", err)
	}
	if _, err := NewCompleteDirectoryResultRoot(catalog.DirectoryID{}, "docs"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero directory error=%v", err)
	}
	if _, err := NewResultRootDirectoryTree(ResultRootLayout{}); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero result-root error=%v", err)
	}
	if _, err := NewZipArchiveArtifact(ResultRootLayout{}); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero zip root error=%v", err)
	}
	if _, err := AppendProtectedSuffix("name", ""); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("empty suffix error=%v", err)
	}
	if _, ok := (ArtifactSpec{}).OriginalFile(); ok || !(ArtifactSpec{}).IsZero() {
		t.Fatal("zero artifact exposed a variant")
	}
}

func TestReservationsBindingsAndPlansAreClosed(t *testing.T) {
	operation := mustIdentity(OperationIDFromBytes(contractStable(20)))
	otherOperation := mustIdentity(OperationIDFromBytes(contractStable(21)))
	reservationID := mustIdentity(DestinationReservationIDFromBytes(contractStable(22)))
	authority := mustIdentity(AuthorityRefFromBytes(contractOpaque(23)))
	workspaceID := mustIdentity(WorkspaceIDFromBytes(contractStable(24)))
	repository := mustIdentity(RepositoryRefFromBytes(contractOpaque(25)))
	portableID := mustIdentity(PortablePlanIDFromBytes(contractStable(26)))
	file := contractID[catalog.FileID](27)
	directory := contractID[catalog.DirectoryID](28)
	original, _ := NewOriginalFileArtifact(file, "report.txt", "report.txt")
	single, _ := NewSingleFileDirectoryTree(file, "report.txt", "report.txt")
	root, _ := NewDirectorySelectionResultRoot(directory, "docs")
	tree, _ := NewResultRootDirectoryTree(root)
	catalogTree := NewCatalogRootDirectoryTree()
	archive, _ := NewZipArchiveArtifact(root)

	nativeRoot, err := NewNativeContainerRootReservation(operation, reservationID, catalogTree, authority)
	if err != nil {
		t.Fatal(err)
	}
	singleName, _ := CollisionName(operation, "report.txt", 1, true)
	nativeEntry, err := NewNativeNamedEntryReservation(operation, reservationID, single, authority, singleName, 1)
	if err != nil {
		t.Fatal(err)
	}
	resultName, _ := CollisionName(operation, "docs-selection", 2, false)
	fsaEntry, err := NewFSANamedEntryReservation(operation, reservationID, tree, authority, resultName, 2)
	if err != nil {
		t.Fatal(err)
	}
	atomicName, _ := CollisionName(operation, "chosen.zip", 3, true)
	atomic, err := NewManagedAtomicReservation(
		operation, reservationID, archive, authority, NameUserChosen, "chosen.zip", atomicName, 3,
	)
	if err != nil {
		t.Fatal(err)
	}

	if nativeRoot.Kind() != ReservationContainerRoot || nativeRoot.OperationID() != operation ||
		nativeRoot.ID() != reservationID || nativeRoot.AuthorityKind() != AuthorityNativeContainer ||
		nativeRoot.AuthorityRef() != authority || nativeRoot.Guarantees() != NativeTreeGuarantees() ||
		nativeRoot.ArtifactDigest() != catalogTree.Digest() || nativeRoot.Digest().IsZero() {
		t.Fatalf("native root=%+v", nativeRoot)
	}
	if nativeEntry.Kind() != ReservationNamedContainerEntry || nativeEntry.EntryKind() != ContainerEntrySingleFile ||
		nativeEntry.RequestedName() != "report.txt" || nativeEntry.ReservedName() != singleName ||
		nativeEntry.CollisionIndex() != 1 || fsaEntry.Guarantees() != FSATreeGuarantees() ||
		atomic.Kind() != ReservationAtomicTarget || atomic.RequestedName() != "chosen.zip" ||
		atomic.ReservedName() != atomicName || atomic.Guarantees().NameAuthority() != NameUserChosen {
		t.Fatal("reservation variant fields mismatch")
	}
	if atomic.Guarantees().Profile() != GuaranteeManagedAtomic ||
		atomic.Guarantees().Replacement() != ReplacementAtomicNoReplace ||
		atomic.Guarantees().Delivery() != DeliveryManagedTarget ||
		atomic.Guarantees().Visibility() != CommitAtomic || atomic.Guarantees().Rollback() != RollbackToAbsent {
		t.Fatalf("atomic guarantees=%+v", atomic.Guarantees())
	}
	if BrowserHandoffGuarantees().Profile() != GuaranteeBrowserHandoff ||
		BrowserHandoffGuarantees().NameAuthority() != NameBrowserChosen {
		t.Fatalf("handoff guarantees=%+v", BrowserHandoffGuarantees())
	}
	if _, err := ManagedAtomicGuarantees(NameBrowserChosen); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("invalid managed guarantee error=%v", err)
	}

	directTree, err := NewDirectTreePlan(catalogTree, nativeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDirectTreePlan(single, nativeEntry); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDirectTreePlan(tree, fsaEntry); err != nil {
		t.Fatal(err)
	}
	directAtomic, err := NewDirectAtomicPlan(archive, atomic)
	if err != nil {
		t.Fatal(err)
	}
	workspaceOriginal, err := NewWorkspaceBinding(operation, workspaceID, original, repository)
	if err != nil {
		t.Fatal(err)
	}
	workspaceArchive, err := NewWorkspaceBinding(operation, workspaceID, archive, repository)
	if err != nil {
		t.Fatal(err)
	}
	workspacePlan, err := NewWorkspaceThenPublishPlan(original, workspaceOriginal)
	if err != nil {
		t.Fatal(err)
	}
	workspaceZipPlan, err := NewWorkspaceThenPublishPlan(archive, workspaceArchive)
	if err != nil {
		t.Fatal(err)
	}
	portable, err := NewPortableBinding(operation, portableID, archive)
	if err != nil {
		t.Fatal(err)
	}
	portablePlan, err := NewPortableHandoffPlan(archive, portable)
	if err != nil {
		t.Fatal(err)
	}

	plans := []struct {
		plan        MaterializationPlan
		kind        MaterializationPlanKind
		preparation PreparationPolicy
	}{
		{directTree, PlanDirectTree, PreparationNone},
		{directAtomic, PlanDirectAtomic, PreparationNone},
		{workspacePlan, PlanWorkspaceThenPublish, PreparationNone},
		{workspaceZipPlan, PlanWorkspaceThenPublish, PreparationExactZip},
		{portablePlan, PlanPortableHandoff, PreparationExactArtifact},
	}
	for _, test := range plans {
		if test.plan.IsZero() || test.plan.Kind() != test.kind || test.plan.Preparation() != test.preparation ||
			test.plan.OperationID() != operation || test.plan.ArtifactDigest().IsZero() ||
			test.plan.BindingDigest().IsZero() || len(test.plan.CanonicalBytes()) == 0 {
			t.Fatalf("plan=%+v", test.plan)
		}
	}
	if value, ok := directTree.DestinationReservation(); !ok || value.Digest() != nativeRoot.Digest() {
		t.Fatalf("direct binding=%+v ok=%v", value, ok)
	}
	if value, ok := workspacePlan.WorkspaceBinding(); !ok || value.WorkspaceID() != workspaceID ||
		value.RepositoryRef() != repository || value.OperationID() != operation ||
		value.ArtifactDigest() != original.Digest() || value.WorkspaceKind() != WorkspaceOriginPrivate ||
		value.BudgetPolicy() != WorkspaceBudgetV1 || value.RetentionPolicy() != WorkspaceStable24HourV1 ||
		value.Digest().IsZero() {
		t.Fatalf("workspace=%+v ok=%v", value, ok)
	}
	if value, ok := portablePlan.PortableBinding(); !ok || value.PortablePlanID() != portableID ||
		value.OperationID() != operation || value.ArtifactDigest() != archive.Digest() ||
		value.MaximumArtifactBytes() != DefaultPortableArtifactLimit ||
		value.AssemblyPartBytes() != DefaultPortableAssemblyPartBytes ||
		value.MaximumParts() != DefaultPortableMaximumParts ||
		value.ObjectURLLeaseMilliseconds() != BrowserHandoffObjectURLLeaseMillis ||
		value.Preparation() != PreparationExactArtifact || value.Digest().IsZero() {
		t.Fatalf("portable=%+v ok=%v", value, ok)
	}
	if _, ok := directTree.WorkspaceBinding(); ok {
		t.Fatal("direct plan exposed workspace binding")
	}
	canonical := atomic.CanonicalBytes()
	canonical[0] ^= 0xff
	if bytes.Equal(canonical, atomic.CanonicalBytes()) || workspaceOriginal.IsZero() || portable.IsZero() {
		t.Fatal("binding canonical storage or validity mismatch")
	}

	if unchanged, err := CollisionName(operation, "report.txt", 0, true); err != nil || unchanged != "report.txt" {
		t.Fatalf("zero collision=%q err=%v", unchanged, err)
	}
	if _, err := CollisionName(OperationID{}, "report.txt", 1, true); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero-operation collision error=%v", err)
	}
	if _, err := NewNativeContainerRootReservation(operation, reservationID, tree, authority); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("result tree accepted as container root: %v", err)
	}
	if _, err := NewNativeNamedEntryReservation(operation, reservationID, single, authority, "wrong.txt", 1); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("wrong collision name accepted: %v", err)
	}
	if _, err := NewManagedAtomicReservation(operation, reservationID, tree, authority, NameApplicationChosen, "docs-selection", "docs-selection", 0); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("tree accepted as atomic artifact: %v", err)
	}
	if _, err := NewManagedAtomicReservation(operation, reservationID, archive, authority, NameApplicationChosen, "renamed.zip", "renamed.zip", 0); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("application-named atomic target accepted a foreign artifact name: %v", err)
	}
	if _, err := NewWorkspaceBinding(operation, workspaceID, tree, repository); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("tree accepted as workspace artifact: %v", err)
	}
	if _, err := NewPortableBinding(operation, portableID, tree); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("tree accepted as portable artifact: %v", err)
	}
	foreignWorkspace, _ := NewWorkspaceBinding(otherOperation, workspaceID, original, repository)
	if _, err := NewWorkspaceThenPublishPlan(archive, foreignWorkspace); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("mismatched workspace accepted: %v", err)
	}
	if _, err := NewPortableHandoffPlan(original, portable); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("mismatched portable binding accepted: %v", err)
	}
	if _, err := NewDirectAtomicPlan(original, atomic); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("mismatched atomic reservation accepted: %v", err)
	}
	if !(MaterializationPlan{}).IsZero() || (MaterializationPlan{}).OperationID() != (OperationID{}) ||
		(MaterializationPlan{}).ArtifactDigest() != (ArtifactDigest{}) ||
		(MaterializationPlan{}).BindingDigest() != (BindingDigest{}) {
		t.Fatal("zero plan exposed authority")
	}
}

func TestReceiveContractArtifactEdgeCases(t *testing.T) {
	file := contractID[catalog.FileID](27)
	directory := contractID[catalog.DirectoryID](28)

	// AppendProtectedSuffix edge cases
	if _, err := AppendProtectedSuffix("", ".zip"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("empty base allowed")
	}
	if _, err := AppendProtectedSuffix(".wsresume", ".zip"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal(".wsresume base allowed")
	}
	if _, err := AppendProtectedSuffix("name", ""); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("empty suffix allowed")
	}
	if _, err := AppendProtectedSuffix("name", "\xff\xfe"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("invalid utf8 suffix allowed")
	}
	if _, err := AppendProtectedSuffix("name", strings.Repeat("a", MaxResultComponentBytes)); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("oversized suffix allowed")
	}

	// NewZipArchiveArtifact with invalid layout
	if _, err := NewZipArchiveArtifact(ResultRootLayout{}); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("invalid layout zip allowed")
	}

	// NewCompleteDirectoryResultRoot & NewDirectorySelectionResultRoot with invalid params
	if _, err := NewCompleteDirectoryResultRoot(catalog.DirectoryID{}, "folder"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("zero directory id allowed")
	}
	if _, err := NewDirectorySelectionResultRoot(catalog.DirectoryID{}, "folder"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("zero directory id allowed")
	}

	// NewOriginalFileArtifact & NewSingleFileDirectoryTree with invalid params
	if _, err := NewOriginalFileArtifact(catalog.FileID{}, "file.txt", "file.txt"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("zero file id original allowed")
	}
	if _, err := NewOriginalFileArtifact(file, "folder/file.txt", "wrong.txt"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("mismatched suggested name allowed")
	}
	if _, err := NewSingleFileDirectoryTree(catalog.FileID{}, "file.txt", "file.txt"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("zero file id single allowed")
	}
	if _, err := NewSingleFileDirectoryTree(file, "folder/file.txt", "wrong.txt"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("mismatched output name allowed")
	}

	// ResultRootLayout getters and zero checks
	root, err := NewDirectorySelectionResultRoot(directory, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if root.Class() != ResultRootDirectorySelection || root.AnchorKind() != ResultRootDirectoryAnchor ||
		root.DirectoryID() != directory || root.SourcePath() != "docs" || root.Name() != "docs-selection" ||
		len(root.CanonicalBytes()) == 0 {
		t.Fatalf("result root getters drifted: %+v", root)
	}
	synthetic := NewSyntheticSelectionResultRoot()
	if synthetic.Class() != ResultRootSyntheticSelection || synthetic.AnchorKind() != ResultRootSyntheticAnchor {
		t.Fatalf("synthetic root getters drifted: %+v", synthetic)
	}
}

