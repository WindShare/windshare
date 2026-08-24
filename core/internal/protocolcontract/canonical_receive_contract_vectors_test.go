package protocolcontract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var retiredReceiveVectorFiles = []string{
	"directory-admission-v1.json",
	"file-checkpoint-v1.json",
	"receive-intent-v1.json",
	"receive-intent-v2.json",
	"receive-intent-v3.json",
	"transfer-intent-v1.json",
}

// These vectors originate in the public Go codecs and are reconstructed through
// the public TypeScript codecs. Inputs stay beside every layer's canonical bytes
// so a mismatch identifies the authority boundary that diverged.
type canonicalContractVectorFile struct {
	Version     int    `json:"version"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Cases       []any  `json:"cases"`
}

type selectionVectorFixture struct {
	spec  transfer.SelectionSpec
	input map[string]any
}

type receiveIntentVectorFixture struct {
	name      string
	selection selectionVectorFixture
	artifact  receivecontract.ArtifactSpec
	plan      receivecontract.MaterializationPlan
	intent    transfer.ReceiveIntent
}

func TestCanonicalReceiveContractVectorFilesUpToDate(t *testing.T) {
	if *update {
		for _, name := range retiredReceiveVectorFiles {
			if err := os.Remove(filepath.Join(vectorsDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("remove retired vector %s: %v", name, err)
			}
		}
	} else {
		for _, name := range retiredReceiveVectorFiles {
			if _, err := os.Stat(filepath.Join(vectorsDir, name)); err == nil || !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("retired vector %s must not be present", name)
			}
		}
	}

	for _, vector := range buildCanonicalReceiveContractVectors(t) {
		encoded, err := json.MarshalIndent(vector, "", "  ")
		if err != nil {
			t.Fatalf("marshal %s: %v", vector.Kind, err)
		}
		encoded = append(encoded, '\n')
		path := filepath.Join(vectorsDir, vector.Kind+".json")
		if *update {
			if err := os.WriteFile(path, encoded, 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (run %s from the repository root): %v", path, updateVectorsCommand, err)
		}
		if !bytes.Equal(committed, encoded) {
			t.Fatalf("%s is stale; run %s from the repository root", path, updateVectorsCommand)
		}
	}
}

func buildCanonicalReceiveContractVectors(t *testing.T) []canonicalContractVectorFile {
	t.Helper()
	intents := buildReceiveIntentVectorFixtures(t)
	intentCases := make([]any, 0, len(intents))
	for _, fixture := range intents {
		intentCases = append(intentCases, receiveIntentVectorCase(fixture))
	}
	return []canonicalContractVectorFile{
		{
			Version: 1,
			Kind:    "artifact-choice-v1",
			Description: "Stable ArtifactChoiceIdentityV1 tuples, including direct-resumable ZIP whose positive route " +
				"support remains confined to the exact reviewed runtime and policy evidence tuple.",
			Cases: buildArtifactChoiceVectorCases(t),
		},
		{
			Version: 1,
			Kind:    "receive-intent-v4",
			Description: "Receiver-local ReceiveIntentV4 values for portable materialization families. Direct resumable ZIP " +
				"is intentionally absent because exact reviewed runtime support is receiver-local, not cross-runtime vector authority.",
			Cases: intentCases,
		},
		{
			Version: 1,
			Kind:    "directory-admission-v2",
			Description: "HMAC-SHA256 DirectoryAdmissionV2 capabilities bound to ReceiveIntentDigest, layout, ancestry, " +
				"generation, canonical path, and modified time.",
			Cases: buildDirectoryAdmissionVectorCases(t, intents),
		},
		{
			Version: 1,
			Kind:    "file-checkpoint-v2",
			Description: "Canonical FileCheckpointV2 records bound to OperationID, ReceiveIntentDigest, materialization " +
				"binding, owned object identity, reducer state, and verified crash cuts, plus the orthogonal local " +
				"CheckpointLineageID index.",
			Cases: buildFileCheckpointVectorCases(t, intents),
		},
	}
}

func buildReceiveIntentVectorFixtures(t *testing.T) []receiveIntentVectorFixture {
	t.Helper()
	share := mustShare(t, contractSequence(0x10, catalog.IdentityBytes))
	root := mustDirectory(t, contractSequence(0x20, catalog.IdentityBytes))
	directory := mustDirectory(t, contractSequence(0x30, catalog.IdentityBytes))
	file := mustFile(t, contractSequence(0x40, catalog.IdentityBytes))

	nodeRules, err := transfer.NewSelectionRules(false, []transfer.SelectionOverride{
		{DirectoryID: directory, Selected: true, Ancestors: []catalog.DirectoryID{root}},
		{FileID: file, Selected: false, Ancestors: []catalog.DirectoryID{root, directory}},
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeSelection := mustSelectionSpec(t, share, root, nodeRules)
	nodeFixture := selectionVectorFixture{
		spec: nodeSelection,
		input: map[string]any{
			"shareInstance": b64URL(share.Bytes()),
			"syntheticRoot": b64URL(root.Bytes()),
			"rules": map[string]any{
				"mode": "node-id", "defaultSelected": false,
				"rules": []any{
					map[string]any{"kind": "directory", "id": b64URL(directory.Bytes()), "selected": true},
					map[string]any{"kind": "file", "id": b64URL(file.Bytes()), "selected": false},
				},
			},
		},
	}

	pathInputs := []string{"photos", "docs/report.txt"}
	pathRules, err := transfer.NewPathSelectionRules(pathInputs)
	if err != nil {
		t.Fatal(err)
	}
	pathFixture := selectionVectorFixture{
		spec: mustSelectionSpec(t, share, root, pathRules),
		input: map[string]any{
			"shareInstance": b64URL(share.Bytes()),
			"syntheticRoot": b64URL(root.Bytes()),
			"rules": map[string]any{
				"mode": "catalog-path", "defaultSelected": false, "paths": pathInputs,
			},
		},
	}

	reportFile := mustFile(t, contractSequence(0x41, catalog.IdentityBytes))
	originalReport, err := receivecontract.NewOriginalFileArtifact(reportFile, "docs/report.txt", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	singleReport, err := receivecontract.NewSingleFileDirectoryTree(reportFile, "docs/report.txt", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	directorySelection, err := receivecontract.NewDirectorySelectionResultRoot(directory, "docs")
	if err != nil {
		t.Fatal(err)
	}
	resultTree, err := receivecontract.NewResultRootDirectoryTree(directorySelection)
	if err != nil {
		t.Fatal(err)
	}
	directoryArchive, err := receivecontract.NewZipArchiveArtifact(directorySelection)
	if err != nil {
		t.Fatal(err)
	}
	syntheticArchive, err := receivecontract.NewZipArchiveArtifact(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	catalogTree := receivecontract.NewCatalogRootDirectoryTree()

	fixtures := make([]receiveIntentVectorFixture, 0, 8)
	fixtures = append(fixtures,
		newNativeCatalogRootFixture(t, "catalog-root-direct-tree", nodeFixture, catalogTree, 0x60),
		newFSANamedFixture(
			t, "single-file-fsa-direct-tree", pathFixture, singleReport, 0x61, 1,
			"report.windshare-abcdef",
		),
		newFSANamedFixture(t, "result-root-fsa-direct-tree", nodeFixture, resultTree, 0x62, 0, "docs-selection"),
		newManagedAtomicFixture(t, "original-file-direct-atomic", pathFixture, originalReport, 0x63, receivecontract.NameApplicationChosen, "report.txt", 0),
		newWorkspaceFixture(t, "original-file-workspace", pathFixture, originalReport, 0x65),
		newWorkspaceFixture(t, "zip-archive-workspace", nodeFixture, syntheticArchive, 0x66),
		newPortableFixture(t, "original-file-portable", pathFixture, originalReport, 0x67),
		newPortableFixture(t, "zip-archive-portable", nodeFixture, directoryArchive, 0x68),
	)
	return fixtures
}

func buildArtifactChoiceVectorCases(t *testing.T) []any {
	t.Helper()
	type choiceCase struct {
		name        string
		artifact    receivecontract.ArtifactKind
		plan        receivecontract.MaterializationPlanKind
		guarantee   receivecontract.GuaranteeProfile
		preparation receivecontract.PreparationPolicy
		support     string
	}
	cases := []choiceCase{
		{"directory-tree-native", receivecontract.ArtifactDirectoryTree, receivecontract.PlanDirectTree, receivecontract.GuaranteeNativeTree, receivecontract.PreparationNone, "available"},
		{"directory-tree-fsa", receivecontract.ArtifactDirectoryTree, receivecontract.PlanDirectTree, receivecontract.GuaranteeFSATree, receivecontract.PreparationNone, "available"},
		{"original-file-direct-atomic", receivecontract.ArtifactOriginalFile, receivecontract.PlanDirectAtomic, receivecontract.GuaranteeManagedAtomic, receivecontract.PreparationNone, "available"},
		{"original-file-workspace-managed", receivecontract.ArtifactOriginalFile, receivecontract.PlanWorkspaceThenPublish, receivecontract.GuaranteeManagedAtomic, receivecontract.PreparationNone, "available"},
		{"original-file-workspace-handoff", receivecontract.ArtifactOriginalFile, receivecontract.PlanWorkspaceThenPublish, receivecontract.GuaranteeBrowserHandoff, receivecontract.PreparationNone, "available"},
		{"zip-workspace-managed", receivecontract.ArtifactZipArchive, receivecontract.PlanWorkspaceThenPublish, receivecontract.GuaranteeManagedAtomic, receivecontract.PreparationExactZip, "available"},
		{"zip-workspace-handoff", receivecontract.ArtifactZipArchive, receivecontract.PlanWorkspaceThenPublish, receivecontract.GuaranteeBrowserHandoff, receivecontract.PreparationExactZip, "available"},
		{"original-file-portable", receivecontract.ArtifactOriginalFile, receivecontract.PlanPortableHandoff, receivecontract.GuaranteeBrowserHandoff, receivecontract.PreparationExactArtifact, "available"},
		{"zip-portable", receivecontract.ArtifactZipArchive, receivecontract.PlanPortableHandoff, receivecontract.GuaranteeBrowserHandoff, receivecontract.PreparationExactArtifact, "available"},
		{"zip-direct-resumable", receivecontract.ArtifactZipArchive, receivecontract.PlanDirectResumableZIP, receivecontract.GuaranteeFSAOwnedFile, receivecontract.PreparationNone, "available-exact-reviewed-platform-only"},
	}
	result := make([]any, 0, len(cases))
	for _, current := range cases {
		identity, err := receivecontract.NewArtifactChoiceIdentity(
			current.artifact, current.plan, current.guarantee, current.preparation,
		)
		if err != nil {
			t.Fatalf("choice %s: %v", current.name, err)
		}
		policyAvailability := any("not-required")
		if current.plan == receivecontract.PlanDirectResumableZIP {
			policyAvailability = map[string]any{
				"directZipEpochPolicyDigest":         directZipEpochPolicyDigestV1(),
				"zipRouteRecommendationPolicyDigest": zipRouteRecommendationPolicyDigestV1(),
				"reason":                             "exact-reviewed-runtime-required",
			}
		}
		result = append(result, map[string]any{
			"name": current.name,
			"input": map[string]any{
				"artifactKind":        artifactKindString(current.artifact),
				"materializationKind": materializationPlanKindString(current.plan),
				"guaranteeProfile":    guaranteeProfileString(current.guarantee),
				"preparation":         preparationPolicyString(current.preparation),
			},
			"routeSupport":       current.support,
			"policyAvailability": policyAvailability,
			"expected": map[string]any{
				"canonicalBytesB64Url": b64URL(identity.CanonicalBytes()),
				"artifactChoiceId":     b64URL(identity.ID().Bytes()),
			},
		})
	}
	return result
}

func newNativeCatalogRootFixture(
	t *testing.T,
	name string,
	selection selectionVectorFixture,
	artifact receivecontract.ArtifactSpec,
	seed byte,
) receiveIntentVectorFixture {
	t.Helper()
	operation := mustOperationID(t, contractSequence(seed, receivecontract.StableIdentityBytes))
	reservationID := mustReservationID(t, contractSequence(seed+0x10, receivecontract.StableIdentityBytes))
	authority := mustAuthorityRef(t, contractSequence(seed+0x20, receivecontract.AuthorityRefBytes))
	reservation, err := receivecontract.NewNativeContainerRootReservation(operation, reservationID, artifact, authority)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	return newReceiveIntentVectorFixture(t, name, selection, artifact, plan)
}

func newFSANamedFixture(
	t *testing.T,
	name string,
	selection selectionVectorFixture,
	artifact receivecontract.ArtifactSpec,
	seed byte,
	collisionIndex uint32,
	physicalName string,
) receiveIntentVectorFixture {
	t.Helper()
	operation := mustOperationID(t, contractSequence(seed, receivecontract.StableIdentityBytes))
	reservationID := mustReservationID(t, contractSequence(seed+0x10, receivecontract.StableIdentityBytes))
	authority := mustAuthorityRef(t, contractSequence(seed+0x20, receivecontract.AuthorityRefBytes))
	requestedName, fileLike := directoryArtifactReservationName(t, artifact)
	reservedName, err := receivecontract.CollisionName(operation, requestedName, collisionIndex, fileLike)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewFSANamedEntryReservation(
		operation, reservationID, artifact, authority, reservedName, physicalName, collisionIndex,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	return newReceiveIntentVectorFixture(t, name, selection, artifact, plan)
}

func newManagedAtomicFixture(
	t *testing.T,
	name string,
	selection selectionVectorFixture,
	artifact receivecontract.ArtifactSpec,
	seed byte,
	nameAuthority receivecontract.NameAuthority,
	requestedName string,
	collisionIndex uint32,
) receiveIntentVectorFixture {
	t.Helper()
	operation := mustOperationID(t, contractSequence(seed, receivecontract.StableIdentityBytes))
	reservationID := mustReservationID(t, contractSequence(seed+0x10, receivecontract.StableIdentityBytes))
	authority := mustAuthorityRef(t, contractSequence(seed+0x20, receivecontract.AuthorityRefBytes))
	reservedName, err := receivecontract.CollisionName(operation, requestedName, collisionIndex, true)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewManagedAtomicReservation(
		operation, reservationID, artifact, authority, nameAuthority,
		requestedName, reservedName, collisionIndex,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectAtomicPlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	return newReceiveIntentVectorFixture(t, name, selection, artifact, plan)
}

func newWorkspaceFixture(
	t *testing.T,
	name string,
	selection selectionVectorFixture,
	artifact receivecontract.ArtifactSpec,
	seed byte,
) receiveIntentVectorFixture {
	t.Helper()
	operation := mustOperationID(t, contractSequence(seed, receivecontract.StableIdentityBytes))
	workspaceID := mustWorkspaceID(t, contractSequence(seed+0x10, receivecontract.StableIdentityBytes))
	repository := mustRepositoryRef(t, contractSequence(seed+0x20, receivecontract.AuthorityRefBytes))
	workspace, err := receivecontract.NewWorkspaceBinding(operation, workspaceID, artifact, repository)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewWorkspaceThenPublishPlan(artifact, workspace)
	if err != nil {
		t.Fatal(err)
	}
	return newReceiveIntentVectorFixture(t, name, selection, artifact, plan)
}

func newPortableFixture(
	t *testing.T,
	name string,
	selection selectionVectorFixture,
	artifact receivecontract.ArtifactSpec,
	seed byte,
) receiveIntentVectorFixture {
	t.Helper()
	operation := mustOperationID(t, contractSequence(seed, receivecontract.StableIdentityBytes))
	portableID := mustPortablePlanID(t, contractSequence(seed+0x10, receivecontract.StableIdentityBytes))
	portable, err := receivecontract.NewPortableBinding(operation, portableID, artifact)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewPortableHandoffPlan(artifact, portable)
	if err != nil {
		t.Fatal(err)
	}
	return newReceiveIntentVectorFixture(t, name, selection, artifact, plan)
}

func newReceiveIntentVectorFixture(
	t *testing.T,
	name string,
	selection selectionVectorFixture,
	artifact receivecontract.ArtifactSpec,
	plan receivecontract.MaterializationPlan,
) receiveIntentVectorFixture {
	t.Helper()
	intent, err := transfer.NewReceiveIntent(selection.spec, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	return receiveIntentVectorFixture{
		name: name, selection: selection, artifact: artifact, plan: plan, intent: intent,
	}
}

func receiveIntentVectorCase(fixture receiveIntentVectorFixture) map[string]any {
	bindingBytes := materializationBindingBytes(fixture.plan)
	choice, err := receivecontract.DeriveArtifactChoiceIdentity(fixture.artifact, fixture.plan)
	if err != nil {
		panic("validated intent cannot derive an artifact choice")
	}
	return map[string]any{
		"name":      fixture.name,
		"selection": fixture.selection.input,
		"artifact":  artifactVectorInput(fixture.artifact),
		"plan":      materializationPlanVectorInput(fixture.plan),
		"expected": map[string]any{
			"selectionCanonicalBytesB64Url":      b64URL(fixture.selection.spec.CanonicalBytes()),
			"selectionDigest":                    b64URL(fixture.selection.spec.Digest().Bytes()),
			"artifactCanonicalBytesB64Url":       b64URL(fixture.artifact.CanonicalBytes()),
			"artifactDigest":                     b64URL(fixture.artifact.Digest().Bytes()),
			"bindingCanonicalBytesB64Url":        b64URL(bindingBytes),
			"bindingDigest":                      b64URL(fixture.plan.BindingDigest().Bytes()),
			"artifactChoiceCanonicalBytesB64Url": b64URL(choice.CanonicalBytes()),
			"artifactChoiceId":                   b64URL(choice.ID().Bytes()),
			"planCanonicalBytesB64Url":           b64URL(fixture.plan.CanonicalBytes()),
			"operationId":                        b64URL(fixture.plan.OperationID().Bytes()),
			"receiveIntentCanonicalBytesB64Url":  b64URL(fixture.intent.CanonicalBytes()),
			"receiveIntentDigest":                b64URL(fixture.intent.Digest().Bytes()),
		},
	}
}

func artifactVectorInput(artifact receivecontract.ArtifactSpec) map[string]any {
	if original, ok := artifact.OriginalFile(); ok {
		return map[string]any{
			"kind": "original-file", "fileId": b64URL(original.FileID.Bytes()),
			"sourcePath": original.SourcePath, "suggestedName": original.SuggestedName,
		}
	}
	if directory, ok := artifact.DirectoryTree(); ok {
		layout := map[string]any{"kind": directoryTreeLayoutKindString(directory.Kind())}
		switch directory.Kind() {
		case receivecontract.DirectoryTreeSingleFile:
			single, _ := directory.SingleFile()
			layout["fileId"] = b64URL(single.FileID.Bytes())
			layout["sourcePath"] = single.SourcePath
			layout["outputName"] = single.SuggestedName
		case receivecontract.DirectoryTreeResultRoot:
			root, _ := directory.ResultRoot()
			layout["root"] = resultRootVectorInput(root)
		case receivecontract.DirectoryTreeCatalogRoot:
		}
		return map[string]any{"kind": "directory-tree", "layout": layout}
	}
	zip, ok := artifact.ZipArchive()
	if !ok {
		panic("validated artifact has no variant")
	}
	return map[string]any{
		"kind": "zip-archive", "layout": resultRootVectorInput(zip.Layout),
		"suggestedName": zip.SuggestedName, "encoding": "store", "completeness": "complete-only",
	}
}

func resultRootVectorInput(layout receivecontract.ResultRootLayout) map[string]any {
	result := map[string]any{
		"class": resultRootClassString(layout.Class()), "name": layout.Name(),
	}
	if layout.AnchorKind() == receivecontract.ResultRootSyntheticAnchor {
		result["anchor"] = map[string]any{"kind": "synthetic-root"}
		return result
	}
	result["anchor"] = map[string]any{
		"kind": "directory", "directoryId": b64URL(layout.DirectoryID().Bytes()),
		"sourcePath": layout.SourcePath(),
	}
	return result
}

func materializationPlanVectorInput(plan receivecontract.MaterializationPlan) map[string]any {
	result := map[string]any{
		"kind": materializationPlanKindString(plan.Kind()), "preparation": preparationPolicyString(plan.Preparation()),
	}
	if reservation, ok := plan.DestinationReservation(); ok {
		result["reservation"] = destinationReservationVectorInput(reservation)
		return result
	}
	if workspace, ok := plan.WorkspaceBinding(); ok {
		result["publicationGuarantee"] = guaranteeProfileString(plan.GuaranteeProfile())
		result["workspace"] = map[string]any{
			"operationId":   b64URL(workspace.OperationID().Bytes()),
			"workspaceId":   b64URL(workspace.WorkspaceID().Bytes()),
			"repositoryRef": b64URL(workspace.RepositoryRef().Bytes()),
			"workspaceKind": "origin-private", "budgetPolicy": "workspace-v1",
			"retentionPolicy": "stable-24h-v1",
		}
		return result
	}
	portable, ok := plan.PortableBinding()
	if ok {
		result["publicationGuarantee"] = "browser-handoff"
		result["portable"] = map[string]any{
			"operationId":                b64URL(portable.OperationID().Bytes()),
			"portablePlanId":             b64URL(portable.PortablePlanID().Bytes()),
			"maximumArtifactBytes":       strconv.FormatUint(portable.MaximumArtifactBytes(), 10),
			"assemblyPartBytes":          strconv.FormatUint(portable.AssemblyPartBytes(), 10),
			"maximumParts":               strconv.FormatUint(portable.MaximumParts(), 10),
			"objectUrlLeaseMilliseconds": strconv.FormatUint(portable.ObjectURLLeaseMilliseconds(), 10),
			"preparation":                "exact-artifact",
		}
		return result
	}
	owned, ok := plan.FSAOwnedFileBinding()
	if !ok {
		panic("validated plan has no binding")
	}
	result["binding"] = fsaOwnedFileBindingVectorInput(owned)
	return result
}

func fsaOwnedFileBindingVectorInput(binding receivecontract.FSAOwnedFileBinding) map[string]any {
	policies := binding.PolicyDigests()
	return map[string]any{
		"operationId": b64URL(binding.OperationID().Bytes()),
		"stableName":  binding.StableName(),
		"targetRef":   b64URL(binding.TargetRef().Bytes()),
		"guarantees":  guaranteeSetVectorInput(binding.Guarantees()),
		"policies": map[string]any{
			"zipEncoding":   b64URL(policies.ZipEncoding.Bytes()),
			"layout":        b64URL(policies.Layout.Bytes()),
			"checkpoint":    b64URL(policies.Checkpoint.Bytes()),
			"journalBudget": b64URL(policies.JournalBudget.Bytes()),
			"epoch":         b64URL(policies.Epoch.Bytes()),
		},
	}
}

func destinationReservationVectorInput(reservation receivecontract.DestinationReservation) map[string]any {
	result := map[string]any{
		"kind":          destinationReservationKindString(reservation.Kind()),
		"operationId":   b64URL(reservation.OperationID().Bytes()),
		"reservationId": b64URL(reservation.ID().Bytes()),
		"authorityKind": authorityKindString(reservation.AuthorityKind()),
		"authorityRef":  b64URL(reservation.AuthorityRef().Bytes()),
		"guarantees":    guaranteeSetVectorInput(reservation.Guarantees()),
	}
	switch reservation.Kind() {
	case receivecontract.ReservationContainerRoot:
	case receivecontract.ReservationNamedContainerEntry:
		result["entryKind"] = containerEntryKindString(reservation.EntryKind())
		result["requestedName"] = reservation.RequestedName()
		result["logicalReservedName"] = reservation.LogicalReservedName()
		result["physicalName"] = reservation.PhysicalName()
		result["collisionIndex"] = reservation.CollisionIndex()
		if layoutVersion, ok := reservation.FSALayoutVersion(); ok {
			result["fsaLayoutVersion"] = layoutVersion
		}
	case receivecontract.ReservationAtomicTarget:
		result["requestedName"] = reservation.RequestedName()
		result["reservedName"] = reservation.ReservedName()
		result["collisionIndex"] = reservation.CollisionIndex()
	}
	return result
}

func guaranteeSetVectorInput(guarantees receivecontract.GuaranteeSet) map[string]any {
	return map[string]any{
		"profile":              guaranteeProfileString(guarantees.Profile()),
		"nameAuthority":        nameAuthorityString(guarantees.NameAuthority()),
		"replacement":          replacementGuaranteeString(guarantees.Replacement()),
		"delivery":             deliveryModeString(guarantees.Delivery()),
		"targetVisibility":     targetVisibilityString(guarantees.TargetVisibility()),
		"artifactAvailability": artifactAvailabilityString(guarantees.ArtifactAvailability()),
		"cleanupAuthority":     cleanupAuthorityString(guarantees.CleanupAuthority()),
	}
}

func materializationBindingBytes(plan receivecontract.MaterializationPlan) []byte {
	if reservation, ok := plan.DestinationReservation(); ok {
		return reservation.CanonicalBytes()
	}
	if workspace, ok := plan.WorkspaceBinding(); ok {
		return workspace.CanonicalBytes()
	}
	if portable, ok := plan.PortableBinding(); ok {
		return portable.CanonicalBytes()
	}
	owned, ok := plan.FSAOwnedFileBinding()
	if !ok {
		panic("validated plan has no binding")
	}
	return owned.CanonicalBytes()
}

func buildDirectoryAdmissionVectorCases(
	t *testing.T,
	intents []receiveIntentVectorFixture,
) []any {
	t.Helper()
	fixture := findReceiveIntentFixture(t, intents, "catalog-root-direct-tree")
	scope, err := transfer.NewDirectoryAdmissionScope(fixture.intent)
	if err != nil {
		t.Fatal(err)
	}
	secret := contractSequence(0xc0, 32)
	rootGeneration := mustGeneration(t, contractSequence(0x50, catalog.IdentityBytes))
	rootDirectory := transfer.AuthenticatedSourceDirectory{
		DirectoryID: fixture.intent.SyntheticRoot(), Generation: rootGeneration,
		SourcePath: ordinaryoutput.EmptySourceCatalogPath(),
	}
	rootPath, err := transfer.NewMaterializationRootRelativePath("")
	if err != nil {
		t.Fatal(err)
	}
	rootMaterialization, err := transfer.NewMaterializationDirectory(
		rootDirectory.DirectoryID, rootDirectory.Generation, rootPath,
		rootDirectory.ParentAdmission, rootDirectory.ModifiedTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	rootAdmission, err := transfer.NewDirectoryAdmissionWithSecret(secret, scope, rootMaterialization)
	if err != nil {
		t.Fatal(err)
	}
	rootSettlement, err := transfer.NewFinalizedDirectorySettlement(rootAdmission)
	if err != nil {
		t.Fatal(err)
	}

	modified, err := catalog.NewModifiedTime(1_700_000_200, 123_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	childPath, err := ordinaryoutput.NewSourceCatalogPath("photos")
	if err != nil {
		t.Fatal(err)
	}
	childDirectory := transfer.AuthenticatedSourceDirectory{
		DirectoryID:     mustDirectory(t, contractSequence(0x30, catalog.IdentityBytes)),
		Generation:      mustGeneration(t, contractSequence(0x60, catalog.IdentityBytes)),
		ParentAdmission: rootAdmission, SourcePath: childPath, ModifiedTime: modified,
	}
	childRelativePath, err := transfer.NewMaterializationRootRelativePath("photos")
	if err != nil {
		t.Fatal(err)
	}
	childMaterialization, err := transfer.NewMaterializationDirectory(
		childDirectory.DirectoryID, childDirectory.Generation, childRelativePath,
		childDirectory.ParentAdmission, childDirectory.ModifiedTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	childAdmission, err := transfer.NewDirectoryAdmissionWithSecret(secret, scope, childMaterialization)
	if err != nil {
		t.Fatal(err)
	}
	directoryMetadataFault, err := fault.NewOutput(fault.ScopeDirectoryLocal, fault.OutputDirectoryMetadata)
	if err != nil {
		t.Fatal(err)
	}
	childSettlement, err := transfer.NewIsolatedDirectorySettlement(childAdmission, directoryMetadataFault)
	if err != nil {
		t.Fatal(err)
	}
	return []any{
		directoryAdmissionVectorCase(
			t, "synthetic-root", nil, fixture.name, rootAdmission, rootSettlement, secret, scope, rootMaterialization,
		),
		directoryAdmissionVectorCase(
			t, "child-generation", "synthetic-root", fixture.name,
			childAdmission, childSettlement, secret, scope, childMaterialization,
		),
	}
}

func directoryAdmissionVectorCase(
	t *testing.T,
	name string,
	parentCase any,
	receiveIntentCase string,
	admission transfer.DirectoryAdmission,
	settlement transfer.DirectorySettlement,
	secret []byte,
	scope transfer.DirectoryAdmissionScope,
	directory transfer.MaterializationDirectory,
) map[string]any {
	t.Helper()
	message, err := transfer.CanonicalDirectoryAdmissionMessageV2(scope, directory)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"name": name, "parentCase": parentCase, "receiveIntentCase": receiveIntentCase,
		"schemaVersion": admission.SchemaVersion(), "secretB64Url": b64URL(secret),
		"scope": map[string]any{
			"receiveIntentDigest": b64URL(scope.ReceiveIntentDigest().Bytes()),
			"layoutVersion":       scope.LayoutVersion(), "layout": directoryAdmissionLayoutString(scope.Layout()),
			"rootExpectation": directoryAdmissionRootVectorInput(scope.RootExpectation()),
		},
		"directory": map[string]any{
			"directoryId":  b64URL(directory.DirectoryID().Bytes()),
			"generation":   b64URL(directory.Generation().Bytes()),
			"path":         canonicalPathSegments(directory.Path().String()),
			"modifiedTime": modifiedTimeVectorInput(directory.ModifiedTime()),
		},
		"messageB64Url": b64URL(message), "token": b64URL(admission.Bytes()),
		"settlement": directorySettlementVectorInput(t, admission, settlement),
	}
}

func directoryAdmissionRootVectorInput(
	root transfer.DirectoryAdmissionRootExpectation,
) map[string]any {
	if root.Kind() == transfer.DirectoryAdmissionNoRoot {
		return map[string]any{"kind": "none", "anchorKind": "single-file"}
	}
	anchorKind := "catalog-root"
	switch root.Kind() {
	case transfer.DirectoryAdmissionDirectoryAnchor:
		anchorKind = "directory"
	case transfer.DirectoryAdmissionSyntheticRoot:
		anchorKind = "synthetic-root"
	}
	return map[string]any{
		"kind": "materialized-directory", "anchorKind": anchorKind,
		"directoryId":  b64URL(root.DirectoryID().Bytes()),
		"relativePath": canonicalPathSegments(root.Path()),
	}
}

func directorySettlementVectorInput(
	t *testing.T,
	admission transfer.DirectoryAdmission,
	settlement transfer.DirectorySettlement,
) map[string]any {
	t.Helper()
	if !settlement.Admission().Equal(admission) {
		t.Fatal("directory settlement does not retain the exact admission")
	}
	result := map[string]any{"admissionToken": b64URL(settlement.Admission().Bytes())}
	switch settlement.Kind() {
	case transfer.DirectoryFinalized:
		result["kind"] = "finalized"
	case transfer.DirectoryIsolatedFailure:
		isolatedFault, ok := settlement.IsolatedFault()
		if !ok {
			t.Fatal("isolated directory settlement omits its normalized fault")
		}
		result["kind"] = "isolated-failure"
		result["fault"] = map[string]any{
			"domain": isolatedFault.Domain().String(),
			"scope":  isolatedFault.Scope().String(),
			"code":   fault.OutputCode(isolatedFault.Code()).String(),
		}
	default:
		t.Fatalf("unsupported directory settlement kind %d", settlement.Kind())
	}
	return result
}

func buildFileCheckpointVectorCases(
	t *testing.T,
	intents []receiveIntentVectorFixture,
) []any {
	t.Helper()
	fixture := findReceiveIntentFixture(t, intents, "catalog-root-direct-tree")
	checkpointFile := mustFile(t, contractSequence(0x21, content.IdentityBytes))
	checkpointRevision, err := content.FileRevisionFromBytes(contractSequence(0x31, content.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	authorityRef := contractSequence(0x41, receivecontract.AuthorityRefBytes)
	ownedObjectID := contractSequence(0x51, receivecontract.AuthorityRefBytes)
	baseSpec := osfs.FileCheckpointSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntentDigest: fixture.intent.Digest(),
		MaterializationBindingDigest: fixture.intent.BindingDigest(),
		FileID:                       checkpointFile, FileRevision: checkpointRevision,
		CanonicalPath: "folder/file.bin", ExactSize: 64,
		MaterializerKind: osfs.FileCheckpointMaterializerNativeTree,
		AuthorityRef:     authorityRef, OwnedObjectID: ownedObjectID,
		StateGeneration: 1, CheckpointGeneration: 1,
		VerifiedRanges: []osfs.FileCheckpointRange{{Offset: 0, End: 16}, {Offset: 32, End: 64}},
		Phase:          osfs.FileCheckpointActive, CommitState: osfs.FileCheckpointCandidate,
	}
	secondRanges := []osfs.FileCheckpointRange{{Offset: 0, End: 16}, {Offset: 24, End: 64}}
	candidate := newFileCheckpointVectorRecord(t, "candidate", baseSpec, nil)
	verified := newFileCheckpointVectorRecord(t, "verified", baseSpec, func(spec *osfs.FileCheckpointSpec) {
		spec.CommitState = osfs.FileCheckpointVerified
	})
	paused := newFileCheckpointVectorRecord(t, "paused", baseSpec, func(spec *osfs.FileCheckpointSpec) {
		spec.StateGeneration = 2
		spec.Phase = osfs.FileCheckpointPaused
		spec.CommitState = osfs.FileCheckpointVerified
	})
	nextCandidate := newFileCheckpointVectorRecord(t, "next candidate", baseSpec, func(spec *osfs.FileCheckpointSpec) {
		spec.StateGeneration = 2
		spec.CheckpointGeneration = 2
		spec.VerifiedRanges = secondRanges
	})
	nextVerified := newFileCheckpointVectorRecord(t, "next verified", baseSpec, func(spec *osfs.FileCheckpointSpec) {
		spec.StateGeneration = 2
		spec.CheckpointGeneration = 2
		spec.VerifiedRanges = secondRanges
		spec.CommitState = osfs.FileCheckpointVerified
	})
	foreignAuthority := newFileCheckpointVectorRecord(t, "foreign authority", baseSpec, func(spec *osfs.FileCheckpointSpec) {
		spec.AuthorityRef = contractSequence(0x42, receivecontract.AuthorityRefBytes)
	})

	result := []any{
		fileCheckpointVectorCase(t, "candidate", fixture.name, candidate),
		fileCheckpointVectorCase(t, "verified", fixture.name, verified),
		fileCheckpointVectorCase(t, "paused", fixture.name, paused),
		fileCheckpointVectorCase(t, "next-candidate", fixture.name, nextCandidate),
		fileCheckpointVectorCase(t, "next-verified", fixture.name, nextVerified),
		fileCheckpointVectorCase(t, "foreign-authority", fixture.name, foreignAuthority),
		map[string]any{
			"name":                             "crash-cuts",
			"beforeCommitRecordId":             b64URL(verified.RecordID().Bytes()),
			"beforeCommitCheckpointGeneration": "1",
			"afterCommitRecordId":              b64URL(nextVerified.RecordID().Bytes()),
			"afterCommitCheckpointGeneration":  "2",
		},
	}
	return append(result, buildFileCheckpointLineageVectorCases(t, fixture.name, baseSpec)...)
}

func findReceiveIntentFixture(
	t *testing.T,
	fixtures []receiveIntentVectorFixture,
	name string,
) receiveIntentVectorFixture {
	t.Helper()
	for _, fixture := range fixtures {
		if fixture.name == name {
			return fixture
		}
	}
	t.Fatalf("receive intent fixture %q is missing", name)
	return receiveIntentVectorFixture{}
}

func directoryArtifactReservationName(
	t *testing.T,
	artifact receivecontract.ArtifactSpec,
) (string, bool) {
	t.Helper()
	layout, ok := artifact.DirectoryTree()
	if !ok {
		t.Fatal("named reservation fixture requires a directory artifact")
	}
	if single, ok := layout.SingleFile(); ok {
		return single.SuggestedName, true
	}
	root, ok := layout.ResultRoot()
	if !ok {
		t.Fatal("named reservation fixture requires a named layout")
	}
	return root.Name(), false
}

func canonicalPathSegments(path string) []string {
	if path == "" {
		return []string{}
	}
	return strings.Split(path, "/")
}

func modifiedTimeVectorInput(modified catalog.ModifiedTime) any {
	if !modified.Present() {
		return nil
	}
	return map[string]any{
		"seconds":     strconv.FormatInt(modified.Seconds(), 10),
		"nanoseconds": modified.Nanoseconds(), "precision": uint8(modified.Precision()),
	}
}

func contractSequence(first byte, count int) []byte {
	result := make([]byte, count)
	for index := range result {
		result[index] = first + byte(index)
	}
	return result
}

func b64URL(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }

func mustSelectionSpec(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules transfer.SelectionRules,
) transfer.SelectionSpec {
	t.Helper()
	value, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustShare(t *testing.T, raw []byte) catalog.ShareInstance {
	t.Helper()
	value, err := catalog.ShareInstanceFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustDirectory(t *testing.T, raw []byte) catalog.DirectoryID {
	t.Helper()
	value, err := catalog.DirectoryIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustGeneration(t *testing.T, raw []byte) catalog.DirectoryGeneration {
	t.Helper()
	value, err := catalog.DirectoryGenerationFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustFile(t *testing.T, raw []byte) catalog.FileID {
	t.Helper()
	value, err := catalog.FileIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustOperationID(t *testing.T, raw []byte) receivecontract.OperationID {
	t.Helper()
	value, err := receivecontract.OperationIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustReservationID(t *testing.T, raw []byte) receivecontract.DestinationReservationID {
	t.Helper()
	value, err := receivecontract.DestinationReservationIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustWorkspaceID(t *testing.T, raw []byte) receivecontract.WorkspaceID {
	t.Helper()
	value, err := receivecontract.WorkspaceIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustPortablePlanID(t *testing.T, raw []byte) receivecontract.PortablePlanID {
	t.Helper()
	value, err := receivecontract.PortablePlanIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustAuthorityRef(t *testing.T, raw []byte) receivecontract.AuthorityRef {
	t.Helper()
	value, err := receivecontract.AuthorityRefFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustRepositoryRef(t *testing.T, raw []byte) receivecontract.RepositoryRef {
	t.Helper()
	value, err := receivecontract.RepositoryRefFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
