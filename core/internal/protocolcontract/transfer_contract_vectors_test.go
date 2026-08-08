package protocolcontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

// These vectors are generated from the public Go codecs. Keeping the input
// values beside the encoded bytes makes the Web test a true cross-runtime
// replay instead of a second hand-written interpretation of the format.
type transferContractVectorFile struct {
	Version     int    `json:"version"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Cases       []any  `json:"cases"`
}

func TestTransferContractVectorFilesUpToDate(t *testing.T) {
	for _, vector := range buildTransferContractVectors(t) {
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
			t.Fatalf("read %s (run go test ./internal/protocolcontract -update): %v", path, err)
		}
		if !bytes.Equal(committed, encoded) {
			t.Fatalf("%s is stale; run go test ./internal/protocolcontract -update", path)
		}
	}
}

func buildTransferContractVectors(t *testing.T) []transferContractVectorFile {
	t.Helper()
	share := mustShare(t, contractSequence(0x10, catalog.IdentityBytes))
	root := mustDirectory(t, contractSequence(0x20, catalog.IdentityBytes))
	descriptor, err := catalog.NewReceivedShareDescriptor(catalog.ReceivedDescriptorSpec{
		WireVersion: catalog.WireVersionV2, Suite: catalog.SuiteV2,
		ShareInstance: share, SyntheticRoot: root, ChunkSize: catalog.DefaultChunkSize,
		Capabilities: catalog.CapabilityCatalog, SenderPublicKey: contractSequence(0x01, catalog.SenderPublicKeySize),
		CreatedAtSeconds: 1, PathPolicy: catalog.PathPolicyV1,
	})
	if err != nil {
		t.Fatal(err)
	}
	share, root = descriptor.ShareInstance(), descriptor.SyntheticRoot()
	// The descriptor transports the synthetic-root identity as an opaque catalog
	// ID. Checking its width here prevents a hash-derived placeholder from
	// silently becoming the cross-runtime intent root.
	const syntheticRootDescriptorBytes = 16
	if raw := root.Bytes(); len(raw) != syntheticRootDescriptorBytes || len(raw) == sha256.Size {
		t.Fatalf("synthetic root descriptor identity has %d bytes, want %d opaque bytes", len(raw), syntheticRootDescriptorBytes)
	}
	directory := mustDirectory(t, contractSequence(0x30, catalog.IdentityBytes))
	file := mustFile(t, contractSequence(0x40, catalog.IdentityBytes))
	generation := mustGeneration(t, contractSequence(0x50, catalog.IdentityBytes))

	nodeRules, err := transfer.NewSelectionRules(false, []transfer.SelectionOverride{
		{DirectoryID: directory, Selected: true, Ancestors: []catalog.DirectoryID{root}},
		{FileID: file, Selected: false, Ancestors: []catalog.DirectoryID{root, directory}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pathInputs := []string{"photos", "docs/re\u0301sume\u0301.txt", "photos", "docs/résumé.txt"}
	pathRules, err := transfer.NewPathSelectionRules(pathInputs)
	if err != nil {
		t.Fatal(err)
	}
	targetIdentity := contractSequence(0x80, transfer.OutputRootIdentityBytes)
	secondTargetIdentity := contractSequence(0xa0, transfer.OutputRootIdentityBytes)
	target, err := transfer.NewOpaqueOutputTarget(targetIdentity)
	if err != nil {
		t.Fatal(err)
	}
	secondTarget, err := transfer.NewOpaqueOutputTarget(secondTargetIdentity)
	if err != nil {
		t.Fatal(err)
	}
	const backend = transfer.OutputBackendID("windshare/native-output")
	intent, err := transfer.NewTransferIntent(share, root, nodeRules, target, backend, transfer.OutputNativeTree)
	if err != nil {
		t.Fatal(err)
	}
	pathIntent, err := transfer.NewTransferIntent(share, root, pathRules, secondTarget, backend, transfer.OutputZIPStream)
	if err != nil {
		t.Fatal(err)
	}
	admissionScope, err := transfer.NewDirectoryAdmissionScope(intent)
	if err != nil {
		t.Fatal(err)
	}

	secret := contractSequence(0xc0, sha256.Size)
	modified, err := catalog.NewModifiedTime(1_700_000_200, 123_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	rootDirectory := transfer.OutputDirectory{
		DirectoryID: root, Generation: generation,
	}
	rootAdmission, err := transfer.NewDirectoryAdmissionWithSecret(secret, admissionScope, rootDirectory)
	if err != nil {
		t.Fatal(err)
	}
	childGeneration := mustGeneration(t, contractSequence(0x60, catalog.IdentityBytes))
	childDirectory := transfer.OutputDirectory{
		DirectoryID: directory, Generation: childGeneration, Path: "photos",
		ParentAdmission: rootAdmission, ModifiedTime: modified,
	}
	childAdmission, err := transfer.NewDirectoryAdmissionWithSecret(secret, admissionScope, childDirectory)
	if err != nil {
		t.Fatal(err)
	}
	rootSettlement, err := transfer.NewFinalizedDirectorySettlement(rootAdmission)
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

	checkpointIntent, err := transfer.TransferIntentDigestFromBytes(contractSequence(0x10, transfer.TransferIntentDigestBytes))
	if err != nil {
		t.Fatal(err)
	}
	checkpointFile := mustFile(t, contractSequence(0x20, content.IdentityBytes))
	checkpointRevision, err := content.FileRevisionFromBytes(contractSequence(0x30, content.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	checkpointRoot := contractSequence(0x40, sha256.Size)
	checkpointObject := contractSequence(0x50, sha256.Size)
	newCheckpoint := func(
		name string,
		rootIdentity []byte,
		stateGeneration uint64,
		checkpointGeneration uint64,
		ranges []osfs.FileCheckpointRange,
		phase osfs.FileCheckpointPhase,
		commitState osfs.FileCheckpointCommitState,
	) osfs.FileCheckpointV1 {
		t.Helper()
		checkpoint, checkpointErr := osfs.NewFileCheckpointV1(osfs.FileCheckpointSpec{
			TransferIntentDigest: checkpointIntent,
			FileID:               checkpointFile,
			FileRevision:         checkpointRevision,
			CanonicalPath:        "folder/file.bin",
			ExactSize:            64,
			BackendID:            "test/native",
			RootIdentity:         rootIdentity,
			OwnedOutputObject:    checkpointObject,
			StateGeneration:      stateGeneration,
			CheckpointGeneration: checkpointGeneration,
			VerifiedRanges:       ranges,
			Phase:                phase,
			CommitState:          commitState,
		})
		if checkpointErr != nil {
			t.Fatalf("%s checkpoint: %v", name, checkpointErr)
		}
		return checkpoint
	}
	firstRanges := []osfs.FileCheckpointRange{{Offset: 0, End: 16}, {Offset: 32, End: 64}}
	secondRanges := []osfs.FileCheckpointRange{{Offset: 0, End: 16}, {Offset: 24, End: 64}}
	candidate := newCheckpoint(
		"candidate", checkpointRoot, 1, 1, firstRanges,
		osfs.FileCheckpointPhaseActive, osfs.FileCheckpointCommitCandidate,
	)
	verified := newCheckpoint(
		"verified", checkpointRoot, 1, 1, firstRanges,
		osfs.FileCheckpointPhaseActive, osfs.FileCheckpointCommitVerified,
	)
	paused := newCheckpoint(
		"paused", checkpointRoot, 2, 1, firstRanges,
		osfs.FileCheckpointPhasePaused, osfs.FileCheckpointCommitVerified,
	)
	nextCandidate := newCheckpoint(
		"next candidate", checkpointRoot, 2, 2, secondRanges,
		osfs.FileCheckpointPhaseActive, osfs.FileCheckpointCommitCandidate,
	)
	nextVerified := newCheckpoint(
		"next verified", checkpointRoot, 2, 2, secondRanges,
		osfs.FileCheckpointPhaseActive, osfs.FileCheckpointCommitVerified,
	)
	foreignRoot := newCheckpoint(
		"foreign root",
		contractSequence(0x41, sha256.Size),
		1,
		1,
		firstRanges,
		osfs.FileCheckpointPhaseActive,
		osfs.FileCheckpointCommitCandidate,
	)
	// Crash-cut expectations are fixtures for cross-runtime codec consumers. The
	// live checkpointstore owns candidate selection and recovery decisions.
	beforeCommit := verified
	afterCommit := nextVerified
	ownership, err := osfs.NewFileCheckpointOwnership(osfs.FileCheckpointOwnershipSpec{
		BackendID:           "test/native",
		Certification:       osfs.FileCheckpointCertificationWindowsNTFSProcessRestart,
		RootIdentity:        checkpointRoot,
		RootOpenDisposition: osfs.FileCheckpointCallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	foreignOwnership, err := osfs.NewFileCheckpointOwnership(osfs.FileCheckpointOwnershipSpec{
		BackendID:           "test/native",
		Certification:       osfs.FileCheckpointCertificationWindowsNTFSProcessRestart,
		RootIdentity:        checkpointRoot,
		RootOpenDisposition: osfs.FileCheckpointAuthorityCreatedRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ownership.CanonicalBytes(), foreignOwnership.CanonicalBytes()) {
		t.Fatal("root disposition is absent from the ownership binding")
	}

	return []transferContractVectorFile{
		{
			Version: 1, Kind: "directory-admission-v1",
			Description: "HMAC-SHA256 DirectoryAdmission V1 receipts and their immutable receipt-bound settlements.",
			Cases: []any{
				directoryAdmissionCase(t, "synthetic-root", rootAdmission, rootSettlement, secret, admissionScope, rootDirectory),
				directoryAdmissionCase(t, "child-generation", childAdmission, childSettlement, secret, admissionScope, childDirectory),
			},
		},
		{
			Version: 1, Kind: "file-checkpoint-v1",
			Description: "Canonical FileCheckpointV1 bindings, phase transitions, certified ownership, and supported crash cuts.",
			Cases: []any{
				fileCheckpointCase(t, "candidate", candidate),
				fileCheckpointCase(t, "verified", verified),
				fileCheckpointCase(t, "paused", paused),
				fileCheckpointCase(t, "next-candidate", nextCandidate),
				fileCheckpointCase(t, "next-verified", nextVerified),
				fileCheckpointCase(t, "foreign-root", foreignRoot),
				fileCheckpointOwnershipCase(t, "ownership", ownership),
				fileCheckpointOwnershipCase(t, "ownership-mismatch", foreignOwnership),
				map[string]any{
					"name":                             "crash-cuts",
					"beforeCommitRecordIdB64":          b64Std(beforeCommit.RecordID().Bytes()),
					"beforeCommitCheckpointGeneration": "1",
					"afterCommitRecordIdB64":           b64Std(afterCommit.RecordID().Bytes()),
					"afterCommitCheckpointGeneration":  "2",
				},
			},
		},
		{
			Version: 1, Kind: "transfer-intent-v1",
			Description: "TransferIntent canonical bytes and SHA-256 digest, excluding run identifiers.",
			Cases: []any{
				transferIntentCase("node-id-persistent-directory", intent, share, root,
					map[string]any{"mode": "node-id", "defaultSelected": false, "rules": []any{
						map[string]any{"kind": "directory", "idB64": b64Std(directory.Bytes()), "selected": true},
						map[string]any{"kind": "file", "idB64": b64Std(file.Bytes()), "selected": false},
					}}, targetIdentity, "directory", backend, contractSequence(0xd0, catalog.IdentityBytes)),
				transferIntentCase("catalog-path-persistent-zip", pathIntent, share, root,
					map[string]any{"mode": "catalog-path", "defaultSelected": false, "inputPaths": pathInputs, "paths": []string{"docs/résumé.txt", "photos"}},
					secondTargetIdentity, "zip", backend, contractSequence(0xe0, catalog.IdentityBytes)),
			},
		},
	}
}

func fileCheckpointCase(
	t *testing.T,
	name string,
	checkpoint osfs.FileCheckpointV1,
) map[string]any {
	t.Helper()
	encoded, err := osfs.EncodeFileCheckpointV1(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	ranges := make([]map[string]string, len(checkpoint.VerifiedRanges()))
	for index, current := range checkpoint.VerifiedRanges() {
		ranges[index] = map[string]string{
			"start": strconv.FormatUint(current.Offset, 10),
			"end":   strconv.FormatUint(current.End, 10),
		}
	}
	return map[string]any{
		"name":                    name,
		"schemaVersion":           checkpoint.SchemaVersion(),
		"ownershipMarker":         checkpoint.OwnershipMarker(),
		"namespace":               checkpoint.Namespace(),
		"recordIdB64":             b64Std(checkpoint.RecordID().Bytes()),
		"transferIntentDigestB64": b64Std(checkpoint.TransferIntentDigest().Bytes()),
		"fileIdB64":               b64Std(checkpoint.FileID().Bytes()),
		"fileRevisionB64":         b64Std(checkpoint.FileRevision().Bytes()),
		"canonicalPath":           checkpoint.CanonicalPath(),
		"exactSize":               strconv.FormatUint(checkpoint.ExactSize(), 10),
		"backend":                 string(checkpoint.BackendID()),
		"rootIdentityB64":         b64Std(checkpoint.RootIdentity().Bytes()),
		"ownedOutputObjectB64":    b64Std(checkpoint.OwnedOutputObject().Bytes()),
		"stateGeneration":         strconv.FormatUint(checkpoint.StateGeneration(), 10),
		"checkpointGeneration":    strconv.FormatUint(checkpoint.CheckpointGeneration(), 10),
		"verifiedRanges":          ranges,
		"phase":                   uint8(checkpoint.Phase()),
		"commitState":             uint8(checkpoint.CommitState()),
		"quarantineReason":        uint8(checkpoint.QuarantineReason()),
		"quarantineOrigin":        uint8(checkpoint.QuarantineOrigin()),
		"retirementReason":        uint8(checkpoint.RetirementReason()),
		"checksumB64":             b64Std(checkpoint.Checksum().Bytes()),
		"canonicalBytesB64":       b64Std(checkpoint.CanonicalBytes()),
		"encodedB64":              b64Std(encoded),
	}
}

func fileCheckpointOwnershipCase(
	t *testing.T,
	name string,
	ownership osfs.FileCheckpointOwnership,
) map[string]any {
	t.Helper()
	encoded, err := osfs.EncodeFileCheckpointOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"name":                name,
		"marker":              osfs.FileCheckpointOwnershipMarker,
		"namespace":           osfs.FileCheckpointNamespace,
		"backend":             string(ownership.BackendID()),
		"certification":       string(ownership.Certification()),
		"rootIdentityB64":     b64Std(ownership.RootIdentity().Bytes()),
		"rootOpenDisposition": string(ownership.RootOpenDisposition()),
		"canonicalBytesB64":   b64Std(ownership.CanonicalBytes()),
		"encodedB64":          b64Std(encoded),
	}
}

func transferIntentCase(name string, intent transfer.TransferIntent, share catalog.ShareInstance, root catalog.DirectoryID, selection map[string]any, target []byte, format string, backend transfer.OutputBackendID, jobID []byte) map[string]any {
	return map[string]any{
		"name": name, "shareInstanceB64": b64Std(share.Bytes()), "syntheticRootB64": b64Std(root.Bytes()),
		"selection":        selection,
		"output":           map[string]any{"targetKind": uint8(intent.OutputTarget().Kind()), "targetIdentityB64": b64Std(target), "backend": string(backend), "format": format},
		"transferJobIdB64": b64Std(jobID), "canonicalBytesB64": b64Std(intent.CanonicalBytes()), "digestB64": b64Std(intent.Digest().Bytes()),
	}
}

func directoryAdmissionCase(
	t *testing.T,
	name string,
	admission transfer.DirectoryAdmission,
	settlement transfer.DirectorySettlement,
	secret []byte,
	scope transfer.DirectoryAdmissionScope,
	directory transfer.OutputDirectory,
) map[string]any {
	t.Helper()
	message, err := transfer.CanonicalDirectoryAdmissionMessageV1(scope, directory)
	if err != nil {
		t.Fatal(err)
	}
	var modified *catalog.ModifiedTime
	if directory.ModifiedTime.Present() {
		value := directory.ModifiedTime
		modified = &value
	}
	var parent []byte
	if !directory.ParentAdmission.IsZero() {
		parent = directory.ParentAdmission.Bytes()
	}
	return map[string]any{
		"name": name, "schemaVersion": admission.SchemaVersion(), "secretB64": b64Std(secret),
		"intentDigestB64": b64Std(scope.IntentDigest().Bytes()), "syntheticRootB64": b64Std(scope.SyntheticRoot().Bytes()),
		"directoryIdB64": b64Std(directory.DirectoryID.Bytes()), "generationB64": b64Std(directory.Generation.Bytes()),
		"path": directory.Path, "parentTokenB64": nullableB64(parent), "modifiedTime": modifiedTimeJSON(modified),
		"messageB64": b64Std(message), "tokenB64": b64Std(admission.Bytes()),
		"settlement": directorySettlementCase(t, admission, settlement),
	}
}

func directorySettlementCase(
	t *testing.T,
	admission transfer.DirectoryAdmission,
	settlement transfer.DirectorySettlement,
) map[string]any {
	t.Helper()
	if !settlement.Admission().Equal(admission) {
		t.Fatal("directory settlement does not retain the exact admission")
	}
	result := map[string]any{"admissionTokenB64": b64Std(settlement.Admission().Bytes())}
	switch settlement.Kind() {
	case transfer.DirectoryFinalized:
		if _, isolated := settlement.IsolatedFault(); isolated {
			t.Fatal("finalized directory settlement exposes an isolated fault")
		}
		result["kind"] = "Finalized"
	case transfer.DirectoryIsolatedFailure:
		isolatedFault, ok := settlement.IsolatedFault()
		if !ok {
			t.Fatal("isolated directory settlement omits its normalized fault")
		}
		result["kind"] = "IsolatedFailure"
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

func modifiedTimeJSON(modified *catalog.ModifiedTime) any {
	if modified == nil {
		return nil
	}
	return map[string]any{"seconds": modified.Seconds(), "nanoseconds": modified.Nanoseconds(), "precision": uint8(modified.Precision())}
}

func nullableB64(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return b64Std(value)
}

func contractSequence(first byte, count int) []byte {
	result := make([]byte, count)
	for index := range result {
		result[index] = first + byte(index)
	}
	return result
}

func b64Std(value []byte) string { return base64.StdEncoding.EncodeToString(value) }

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
