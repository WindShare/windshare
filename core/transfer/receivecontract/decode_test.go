package receivecontract

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestCanonicalDecodersRoundTripEveryClosedArtifactAndPlan(t *testing.T) {
	fixture := newDecoderContractFixture(t)
	for name, artifact := range fixture.artifacts {
		t.Run("artifact-"+name, func(t *testing.T) {
			decoded, err := DecodeArtifactSpec(artifact.CanonicalBytes())
			if err != nil || decoded.Kind() != artifact.Kind() || decoded.Digest() != artifact.Digest() ||
				!bytes.Equal(decoded.CanonicalBytes(), artifact.CanonicalBytes()) {
				t.Fatalf("decoded artifact kind=%d digest=%x error=%v", decoded.Kind(), decoded.Digest(), err)
			}
		})
	}
	for name, candidate := range fixture.plans {
		t.Run("plan-"+name, func(t *testing.T) {
			decoded, err := DecodeMaterializationPlan(candidate.plan.CanonicalBytes(), candidate.artifact)
			if err != nil || decoded.Kind() != candidate.plan.Kind() ||
				decoded.OperationID() != candidate.plan.OperationID() ||
				decoded.BindingDigest() != candidate.plan.BindingDigest() ||
				!bytes.Equal(decoded.CanonicalBytes(), candidate.plan.CanonicalBytes()) {
				t.Fatalf("decoded plan kind=%d operation=%x error=%v", decoded.Kind(), decoded.OperationID(), err)
			}
		})
	}
}

func TestCanonicalDecodersRejectMalformedNonCanonicalAndMismatchedImages(t *testing.T) {
	fixture := newDecoderContractFixture(t)
	original := fixture.artifacts["original"]
	artifactBytes := original.CanonicalBytes()
	zeroFile := bytes.Clone(artifactBytes)
	fileOffset := bytes.Index(zeroFile, original.original.FileID.Bytes())
	if fileOffset < 0 {
		t.Fatal("file identity is absent from canonical artifact")
	}
	clear(zeroFile[fileOffset : fileOffset+catalog.IdentityBytes])
	wrongVersion := bytes.Clone(artifactBytes)
	wrongVersion[len(artifactSpecDomain)+1]++
	unknownArtifact := bytes.Clone(artifactBytes)
	unknownArtifact[len(artifactSpecDomain)+2] = 0xff
	wrongName := bytes.Clone(artifactBytes)
	wrongName[len(wrongName)-1] ^= 1
	for name, encoded := range map[string][]byte{
		"empty":          nil,
		"wrong-version":  wrongVersion,
		"unknown-kind":   unknownArtifact,
		"truncated":      artifactBytes[:len(artifactBytes)-1],
		"trailing":       append(bytes.Clone(artifactBytes), 0),
		"zero-file":      zeroFile,
		"derived-name":   wrongName,
		"foreign-prefix": append([]byte("foreign"), artifactBytes...),
	} {
		t.Run("artifact-"+name, func(t *testing.T) {
			if _, err := DecodeArtifactSpec(encoded); !errors.Is(err, ErrInvalidReceiveContract) {
				t.Fatalf("decode error=%v", err)
			}
		})
	}

	direct := fixture.plans["direct-tree-root"]
	planBytes := direct.plan.CanonicalBytes()
	unknownPlan := bytes.Clone(planBytes)
	unknownPlan[len(materializationPlanDomain)+2] = 0xff
	wrongPreparation := bytes.Clone(planBytes)
	wrongPreparation[len(wrongPreparation)-1] = byte(PreparationExactArtifact)
	zeroOperation := bytes.Clone(planBytes)
	operationOffset := bytes.Index(zeroOperation, direct.plan.OperationID().Bytes())
	if operationOffset < 0 {
		t.Fatal("operation identity is absent from canonical plan")
	}
	clear(zeroOperation[operationOffset : operationOffset+StableIdentityBytes])
	invalidGuarantee := bytes.Clone(planBytes)
	reservation, ok := direct.plan.DestinationReservation()
	if !ok {
		t.Fatal("direct plan has no reservation")
	}
	guaranteeOffset := bytes.Index(invalidGuarantee, reservation.guarantees.canonicalBytes())
	if guaranteeOffset < 0 {
		t.Fatal("guarantee profile is absent from canonical plan")
	}
	invalidGuarantee[guaranteeOffset+len(reservation.guarantees.canonicalBytes())-1] = 0xff
	for name, test := range map[string]struct {
		encoded  []byte
		artifact ArtifactSpec
	}{
		"empty":               {nil, direct.artifact},
		"unknown-plan":        {unknownPlan, direct.artifact},
		"wrong-preparation":   {wrongPreparation, direct.artifact},
		"zero-operation":      {zeroOperation, direct.artifact},
		"invalid-guarantee":   {invalidGuarantee, direct.artifact},
		"truncated":           {planBytes[:len(planBytes)-1], direct.artifact},
		"trailing":            {append(bytes.Clone(planBytes), 0), direct.artifact},
		"mismatched-artifact": {planBytes, fixture.artifacts["zip-selection"]},
		"zero-artifact":       {planBytes, ArtifactSpec{}},
	} {
		t.Run("plan-"+name, func(t *testing.T) {
			if _, err := DecodeMaterializationPlan(test.encoded, test.artifact); !errors.Is(err, ErrInvalidReceiveContract) {
				t.Fatalf("decode error=%v", err)
			}
		})
	}

	for offset := range len(artifactBytes) {
		if _, err := DecodeArtifactSpec(artifactBytes[:offset]); !errors.Is(err, ErrInvalidReceiveContract) {
			t.Fatalf("artifact truncation %d error=%v", offset, err)
		}
	}
	for offset := range len(planBytes) {
		if _, err := DecodeMaterializationPlan(planBytes[:offset], direct.artifact); !errors.Is(err, ErrInvalidReceiveContract) {
			t.Fatalf("plan truncation %d error=%v", offset, err)
		}
	}
}

func TestNestedCanonicalDecodersRejectOpenUnionsAndPolicyDrift(t *testing.T) {
	fixture := newDecoderContractFixture(t)
	original := fixture.artifacts["original"]
	for name, decode := range map[string]func() error{
		"empty-tree-layout": func() error { _, err := decodeDirectoryTreeArtifact(nil); return err },
		"short-single-layout": func() error {
			_, err := decodeDirectoryTreeArtifact([]byte{byte(DirectoryTreeSingleFile)})
			return err
		},
		"short-result-layout": func() error {
			_, err := decodeDirectoryTreeArtifact([]byte{byte(DirectoryTreeResultRoot)})
			return err
		},
		"unknown-tree-layout": func() error { _, err := decodeDirectoryTreeArtifact([]byte{0xff}); return err },
		"trailing-catalog-layout": func() error {
			_, err := decodeDirectoryTreeArtifact([]byte{byte(DirectoryTreeCatalogRoot), 0})
			return err
		},
		"empty-result-root": func() error { _, err := decodeResultRootLayout(nil); return err },
		"empty-reservation": func() error {
			_, err := decodeDestinationReservation(nil, original)
			return err
		},
		"empty-workspace": func() error { _, err := decodeWorkspaceBinding(nil, original); return err },
		"empty-portable":  func() error { _, err := decodePortableBinding(nil, original); return err },
		"empty-path":      func() error { _, err := decodeCanonicalPath(nil); return err },
		"missing-path-frame": func() error {
			_, err := decodeCanonicalPath(uint64Bytes(1))
			return err
		},
		"invalid-path": func() error {
			_, err := decodeCanonicalPath(canonicalPathBytes("../report.txt"))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := decode(); !errors.Is(err, ErrInvalidReceiveContract) {
				t.Fatalf("decode error=%v", err)
			}
		})
	}

	directory := contractID[catalog.DirectoryID](101)
	directoryAnchor := append([]byte{byte(ResultRootDirectoryAnchor)}, frame(directory.Bytes())...)
	directoryAnchor = append(directoryAnchor, frame(canonicalPathBytes("docs"))...)
	for name, encoded := range map[string][]byte{
		"missing-anchor": canonicalRecord(resultRootLayoutDomain,
			frame([]byte{byte(ResultRootSyntheticSelection)}), frame(nil), frame([]byte(DefaultResultRootName))),
		"unknown-anchor": canonicalRecord(resultRootLayoutDomain,
			frame([]byte{byte(ResultRootSyntheticSelection)}), frame([]byte{0xff}), frame([]byte(DefaultResultRootName))),
		"wrong-directory-class": canonicalRecord(resultRootLayoutDomain,
			frame([]byte{byte(ResultRootSyntheticSelection)}), frame(directoryAnchor), frame([]byte("docs"))),
		"wrong-synthetic-class": canonicalRecord(resultRootLayoutDomain,
			frame([]byte{byte(ResultRootCompleteDirectory)}), frame([]byte{byte(ResultRootSyntheticAnchor)}), frame([]byte("docs"))),
		"zero-directory": canonicalRecord(resultRootLayoutDomain,
			frame([]byte{byte(ResultRootCompleteDirectory)}),
			frame(append(append([]byte{byte(ResultRootDirectoryAnchor)}, frame(make([]byte, catalog.IdentityBytes))...), frame(canonicalPathBytes("docs"))...)),
			frame([]byte("docs"))),
		"wrong-name": canonicalRecord(resultRootLayoutDomain,
			frame([]byte{byte(ResultRootCompleteDirectory)}), frame(directoryAnchor), frame([]byte("other"))),
	} {
		t.Run("result-root-"+name, func(t *testing.T) {
			if _, err := decodeResultRootLayout(encoded); !errors.Is(err, ErrInvalidReceiveContract) {
				t.Fatalf("decode error=%v", err)
			}
		})
	}

	direct := fixture.plans["direct-tree-root"]
	rootReservation, ok := direct.plan.DestinationReservation()
	if !ok {
		t.Fatal("direct plan has no reservation")
	}
	wrongRootAuthority := decoderReservationImage(
		rootReservation, ReservationContainerRoot, AuthorityFSAContainer,
		NativeTreeGuarantees(), 0, "", "", 0,
	)
	unknownReservation := decoderReservationImage(
		rootReservation, DestinationReservationKind(0xff), AuthorityNativeContainer,
		NativeTreeGuarantees(), 0, "", "", 0,
	)
	for name, encoded := range map[string][]byte{
		"wrong-root-authority": wrongRootAuthority,
		"unknown-kind":         unknownReservation,
		"trailing":             append(rootReservation.CanonicalBytes(), 0),
	} {
		t.Run("reservation-"+name, func(t *testing.T) {
			if _, err := decodeDestinationReservation(encoded, direct.artifact); !errors.Is(err, ErrInvalidReceiveContract) {
				t.Fatalf("decode error=%v", err)
			}
		})
	}

	named := fixture.plans["direct-tree-single"]
	namedReservation, ok := named.plan.DestinationReservation()
	if !ok {
		t.Fatal("named plan has no reservation")
	}
	for name, encoded := range map[string][]byte{
		"missing-fields": decoderReservationImage(
			namedReservation, ReservationNamedContainerEntry, AuthorityNativeContainer,
			NativeTreeGuarantees(), 0, "", "", 0,
		)[:len(rootReservation.CanonicalBytes())],
		"wrong-guarantee": decoderReservationImage(
			namedReservation, ReservationNamedContainerEntry, AuthorityNativeContainer,
			FSATreeGuarantees(), namedReservation.EntryKind(), namedReservation.RequestedName(),
			namedReservation.LogicalReservedName(), namedReservation.CollisionIndex(),
		),
		"unknown-authority": decoderReservationImage(
			namedReservation, ReservationNamedContainerEntry, AuthorityKind(0xff),
			NativeTreeGuarantees(), namedReservation.EntryKind(), namedReservation.RequestedName(),
			namedReservation.LogicalReservedName(), namedReservation.CollisionIndex(),
		),
		"wrong-entry-kind": decoderReservationImage(
			namedReservation, ReservationNamedContainerEntry, AuthorityNativeContainer,
			NativeTreeGuarantees(), ContainerEntryResultRoot, namedReservation.RequestedName(),
			namedReservation.LogicalReservedName(), namedReservation.CollisionIndex(),
		),
		"wrong-requested-name": decoderReservationImage(
			namedReservation, ReservationNamedContainerEntry, AuthorityNativeContainer,
			NativeTreeGuarantees(), namedReservation.EntryKind(), "other.txt",
			namedReservation.LogicalReservedName(), namedReservation.CollisionIndex(),
		),
	} {
		t.Run("named-reservation-"+name, func(t *testing.T) {
			if _, err := decodeDestinationReservation(encoded, named.artifact); !errors.Is(err, ErrInvalidReceiveContract) {
				t.Fatalf("decode error=%v", err)
			}
		})
	}

	fsa := fixture.plans["direct-tree-selection"]
	fsaReservation, ok := fsa.plan.DestinationReservation()
	if !ok {
		t.Fatal("FSA plan has no reservation")
	}
	fsaWrongGuarantee := decoderReservationImage(
		fsaReservation, ReservationNamedContainerEntry, AuthorityFSAContainer,
		NativeTreeGuarantees(), fsaReservation.EntryKind(), fsaReservation.RequestedName(),
		fsaReservation.LogicalReservedName(), fsaReservation.CollisionIndex(),
	)
	fsaWrongLayout := fsaReservation.CanonicalBytes()
	fsaWrongLayout[len(fsaWrongLayout)-1] = 0
	fsaMissingLayout := fsaReservation.CanonicalBytes()
	layoutFieldBytes := len(frame([]byte{FSAReservedRootLayoutV1}))
	fsaMissingLayout = fsaMissingLayout[:len(fsaMissingLayout)-layoutFieldBytes]
	for name, encoded := range map[string][]byte{
		"wrong-guarantee": fsaWrongGuarantee,
		"wrong-layout":    fsaWrongLayout,
		"missing-layout":  fsaMissingLayout,
	} {
		t.Run("FSA-reservation-"+name, func(t *testing.T) {
			if _, err := decodeDestinationReservation(encoded, fsa.artifact); !errors.Is(err, ErrInvalidReceiveContract) {
				t.Fatalf("decode error=%v", err)
			}
		})
	}

	atomic := fixture.plans["direct-atomic-original"]
	atomicReservation, ok := atomic.plan.DestinationReservation()
	if !ok {
		t.Fatal("atomic plan has no reservation")
	}
	atomicWrongAuthority := decoderReservationImage(
		atomicReservation, ReservationAtomicTarget, AuthorityNativeContainer,
		atomicReservation.Guarantees(), 0, atomicReservation.RequestedName(),
		atomicReservation.ReservedName(), atomicReservation.CollisionIndex(),
	)
	if _, err := decodeDestinationReservation(atomicWrongAuthority, atomic.artifact); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("atomic authority error=%v", err)
	}

	workspace := fixture.plans["workspace-original"]
	workspaceBinding, ok := workspace.plan.WorkspaceBinding()
	if !ok {
		t.Fatal("workspace plan has no binding")
	}
	workspaceTrailing := append(workspaceBinding.CanonicalBytes(), 0)
	workspacePolicy := workspaceBinding.CanonicalBytes()
	workspacePolicy[len(workspacePolicy)-1] = 0xff
	for _, encoded := range [][]byte{workspaceTrailing, workspacePolicy} {
		if _, err := decodeWorkspaceBinding(encoded, workspace.artifact); !errors.Is(err, ErrInvalidReceiveContract) {
			t.Fatalf("workspace decode error=%v", err)
		}
	}

	portable := fixture.plans["portable-original"]
	portableBinding, ok := portable.plan.PortableBinding()
	if !ok {
		t.Fatal("portable plan has no binding")
	}
	portableTrailing := append(portableBinding.CanonicalBytes(), 0)
	portablePolicy := portableBinding.CanonicalBytes()
	portablePolicy[len(portablePolicy)-1] = 0xff
	for _, encoded := range [][]byte{portableTrailing, portablePolicy} {
		if _, err := decodePortableBinding(encoded, portable.artifact); !errors.Is(err, ErrInvalidReceiveContract) {
			t.Fatalf("portable decode error=%v", err)
		}
	}

	workspacePlan := workspace.plan.CanonicalBytes()
	workspacePlan[len(workspacePlan)-1] = byte(PreparationExactZip)
	if _, err := DecodeMaterializationPlan(workspacePlan, workspace.artifact); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("workspace preparation error=%v", err)
	}
	portablePlan := portable.plan.CanonicalBytes()
	portablePlan[len(portablePlan)-10] = byte(GuaranteeManagedAtomic)
	if _, err := DecodeMaterializationPlan(portablePlan, portable.artifact); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("portable route error=%v", err)
	}

	tooLargePath := uint64Bytes(129)
	for range 129 {
		tooLargePath = append(tooLargePath, frame(bytes.Repeat([]byte{'a'}, catalog.MaxNameBytes))...)
	}
	if _, err := decodeCanonicalPath(tooLargePath); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("oversize path error=%v", err)
	}
}

func decoderReservationImage(
	reservation DestinationReservation,
	kind DestinationReservationKind,
	authorityKind AuthorityKind,
	guarantees GuaranteeSet,
	entryKind ContainerEntryKind,
	requestedName, reservedName string,
	collisionIndex uint32,
) []byte {
	encoded := canonicalRecord(destinationReservationDomain,
		[]byte{byte(kind)},
		frame(reservation.OperationID().Bytes()), frame(reservation.ID().Bytes()),
		frame(reservation.ArtifactDigest().Bytes()), frame([]byte{byte(authorityKind)}),
		frame(reservation.AuthorityRef().Bytes()), frame(guarantees.canonicalBytes()),
	)
	switch kind {
	case ReservationNamedContainerEntry:
		encoded = append(encoded, frame([]byte{byte(entryKind)})...)
		encoded = append(encoded, frame([]byte(requestedName))...)
		encoded = append(encoded, frame([]byte(reservedName))...)
		encoded = append(encoded, frame([]byte(reservation.PhysicalName()))...)
		encoded = append(encoded, frame(uint32Bytes(collisionIndex))...)
		if authorityKind == AuthorityFSAContainer {
			encoded = append(encoded, frame([]byte{FSAReservedRootLayoutV1})...)
		}
	case ReservationAtomicTarget:
		encoded = append(encoded, frame([]byte(requestedName))...)
		encoded = append(encoded, frame([]byte(reservedName))...)
		encoded = append(encoded, frame(uint32Bytes(collisionIndex))...)
	}
	return encoded
}

type decoderPlanFixture struct {
	artifact ArtifactSpec
	plan     MaterializationPlan
}

type decoderContractFixture struct {
	artifacts map[string]ArtifactSpec
	plans     map[string]decoderPlanFixture
}

func newDecoderContractFixture(t *testing.T) decoderContractFixture {
	t.Helper()
	operation := mustIdentity(OperationIDFromBytes(contractStable(61)))
	reservationID := mustIdentity(DestinationReservationIDFromBytes(contractStable(62)))
	workspaceID := mustIdentity(WorkspaceIDFromBytes(contractStable(63)))
	portableID := mustIdentity(PortablePlanIDFromBytes(contractStable(64)))
	authority := mustIdentity(AuthorityRefFromBytes(contractOpaque(65)))
	repository := mustIdentity(RepositoryRefFromBytes(contractOpaque(66)))
	file := contractID[catalog.FileID](67)
	directory := contractID[catalog.DirectoryID](68)

	original, err := NewOriginalFileArtifact(file, "docs/report.txt", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	single, err := NewSingleFileDirectoryTree(file, "docs/report.txt", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	completeRoot, err := NewCompleteDirectoryResultRoot(directory, "docs")
	if err != nil {
		t.Fatal(err)
	}
	selectionRoot, err := NewDirectorySelectionResultRoot(directory, "docs")
	if err != nil {
		t.Fatal(err)
	}
	syntheticRoot := NewSyntheticSelectionResultRoot()
	completeTree, err := NewResultRootDirectoryTree(completeRoot)
	if err != nil {
		t.Fatal(err)
	}
	selectionTree, err := NewResultRootDirectoryTree(selectionRoot)
	if err != nil {
		t.Fatal(err)
	}
	syntheticTree, err := NewResultRootDirectoryTree(syntheticRoot)
	if err != nil {
		t.Fatal(err)
	}
	catalogTree := NewCatalogRootDirectoryTree()
	zipComplete, err := NewZipArchiveArtifact(completeRoot)
	if err != nil {
		t.Fatal(err)
	}
	zipSelection, err := NewZipArchiveArtifact(selectionRoot)
	if err != nil {
		t.Fatal(err)
	}
	zipSynthetic, err := NewZipArchiveArtifact(syntheticRoot)
	if err != nil {
		t.Fatal(err)
	}

	rootReservation, err := NewNativeContainerRootReservation(operation, reservationID, catalogTree, authority)
	if err != nil {
		t.Fatal(err)
	}
	singleReserved, err := CollisionName(operation, "report.txt", 1, true)
	if err != nil {
		t.Fatal(err)
	}
	singleReservation, err := NewNativeNamedEntryReservation(
		operation, reservationID, single, authority, singleReserved, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	selectionReserved, err := CollisionName(operation, selectionRoot.Name(), 2, false)
	if err != nil {
		t.Fatal(err)
	}
	selectionReservation, err := NewFSANamedEntryReservation(
		operation, reservationID, selectionTree, authority, selectionReserved, selectionReserved, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	originalReserved, err := CollisionName(operation, "report.txt", 3, true)
	if err != nil {
		t.Fatal(err)
	}
	originalReservation, err := NewManagedAtomicReservation(
		operation, reservationID, original, authority, NameApplicationChosen,
		"report.txt", originalReserved, 3,
	)
	if err != nil {
		t.Fatal(err)
	}
	target, err := FSAOwnedTargetRefFromBytes(contractOpaque(140))
	if err != nil {
		t.Fatal(err)
	}
	policies := DirectZipPolicyDigests{
		ZipEncoding:   mustPolicyDigest(t, contractOpaque(150)),
		Layout:        mustPolicyDigest(t, contractOpaque(160)),
		Checkpoint:    mustPolicyDigest(t, contractOpaque(170)),
		JournalBudget: mustPolicyDigest(t, contractOpaque(180)),
		Epoch:         mustPolicyDigest(t, contractOpaque(190)),
	}
	ownedFile, err := NewFSAOwnedFileBinding(
		operation, zipSelection, "docs-selection.windshare-YWJjZGVmZ2hpamtsbW5vcA.zip", target, policies,
	)
	if err != nil {
		t.Fatal(err)
	}

	directRoot, err := NewDirectTreePlan(catalogTree, rootReservation)
	if err != nil {
		t.Fatal(err)
	}
	directSingle, err := NewDirectTreePlan(single, singleReservation)
	if err != nil {
		t.Fatal(err)
	}
	directSelection, err := NewDirectTreePlan(selectionTree, selectionReservation)
	if err != nil {
		t.Fatal(err)
	}
	atomicOriginal, err := NewDirectAtomicPlan(original, originalReservation)
	if err != nil {
		t.Fatal(err)
	}
	directZip, err := NewDirectResumableZIPPlan(zipSelection, ownedFile)
	if err != nil {
		t.Fatal(err)
	}
	workspaceOriginalBinding, err := NewWorkspaceBinding(operation, workspaceID, original, repository)
	if err != nil {
		t.Fatal(err)
	}
	workspaceZipBinding, err := NewWorkspaceBinding(operation, workspaceID, zipComplete, repository)
	if err != nil {
		t.Fatal(err)
	}
	workspaceOriginal, err := NewWorkspaceThenPublishPlan(original, workspaceOriginalBinding)
	if err != nil {
		t.Fatal(err)
	}
	workspaceZip, err := NewWorkspaceThenPublishPlan(zipComplete, workspaceZipBinding)
	if err != nil {
		t.Fatal(err)
	}
	portableOriginalBinding, err := NewPortableBinding(operation, portableID, original)
	if err != nil {
		t.Fatal(err)
	}
	portableZipBinding, err := NewPortableBinding(operation, portableID, zipSynthetic)
	if err != nil {
		t.Fatal(err)
	}
	portableOriginal, err := NewPortableHandoffPlan(original, portableOriginalBinding)
	if err != nil {
		t.Fatal(err)
	}
	portableZip, err := NewPortableHandoffPlan(zipSynthetic, portableZipBinding)
	if err != nil {
		t.Fatal(err)
	}

	return decoderContractFixture{
		artifacts: map[string]ArtifactSpec{
			"original": original, "tree-single": single, "tree-complete": completeTree,
			"tree-selection": selectionTree, "tree-synthetic": syntheticTree,
			"tree-catalog": catalogTree, "zip-complete": zipComplete,
			"zip-selection": zipSelection, "zip-synthetic": zipSynthetic,
		},
		plans: map[string]decoderPlanFixture{
			"direct-tree-root":       {catalogTree, directRoot},
			"direct-tree-single":     {single, directSingle},
			"direct-tree-selection":  {selectionTree, directSelection},
			"direct-atomic-original": {original, atomicOriginal},
			"direct-resumable-zip":   {zipSelection, directZip},
			"workspace-original":     {original, workspaceOriginal},
			"workspace-zip":          {zipComplete, workspaceZip},
			"portable-original":      {original, portableOriginal},
			"portable-zip":           {zipSynthetic, portableZip},
		},
	}
}

func mustPolicyDigest(t *testing.T, raw []byte) PolicyDigest {
	t.Helper()
	value, err := PolicyDigestFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
