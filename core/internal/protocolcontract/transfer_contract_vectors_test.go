package protocolcontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
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

	secret := contractSequence(0xc0, sha256.Size)
	modified, err := catalog.NewModifiedTime(1_700_000_200, 123_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	rootAdmission, err := transfer.NewDirectoryAdmissionWithSecret(secret, transfer.OutputDirectory{
		DirectoryID: root, Generation: generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	childGeneration := mustGeneration(t, contractSequence(0x60, catalog.IdentityBytes))
	childAdmission, err := transfer.NewDirectoryAdmissionWithSecret(secret, transfer.OutputDirectory{
		DirectoryID: directory, Generation: childGeneration, Path: "photos",
		ParentAdmission: rootAdmission, ModifiedTime: modified,
	})
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
	checkpoint, err := osfs.NewFileCheckpointV1(osfs.FileCheckpointSpec{
		TransferIntentDigest: checkpointIntent,
		FileID:               checkpointFile,
		FileRevision:         checkpointRevision,
		CanonicalPath:        "folder/file.bin",
		ExactSize:            64,
		BackendID:            "test/native",
		RootIdentity:         checkpointRoot,
		OwnedOutputObject:    checkpointObject,
		StateGeneration:      1,
		CheckpointGeneration: 1,
		VerifiedRanges: []osfs.FileCheckpointRange{
			{Offset: 0, End: 16}, {Offset: 32, End: 64},
		},
		Phase:       osfs.FileCheckpointPhaseActive,
		CommitState: osfs.FileCheckpointCommitCandidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpointEncoded, err := osfs.EncodeFileCheckpointV1(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := osfs.NewFileCheckpointOwnership("test/native", checkpointRoot)
	if err != nil {
		t.Fatal(err)
	}
	ownershipEncoded, err := osfs.EncodeFileCheckpointOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}

	return []transferContractVectorFile{
		{
			Version: 1, Kind: "directory-admission-v1",
			Description: "Deterministic session-secret-scoped DirectoryAdmission proofs, including modified-time and parent binding.",
			Cases: []any{
				directoryAdmissionCase("synthetic-root", rootAdmission, secret, root, generation, "", nil, nil),
				directoryAdmissionCase("child-generation", childAdmission, secret, directory, childGeneration, "photos", &modified, rootAdmission.Bytes()),
			},
		},
		{
			Version: 1, Kind: "file-checkpoint-v1",
			Description: "FileCheckpointV1 canonical payload, storage envelope, ownership marker, and checksum.",
			Cases: []any{
				map[string]any{
					"name":            "candidate",
					"ownershipMarker": checkpoint.OwnershipMarker(), "namespace": checkpoint.Namespace(),
					"recordIdB64":             b64Std(checkpoint.RecordID().Bytes()),
					"transferIntentDigestB64": b64Std(checkpoint.TransferIntentDigest().Bytes()),
					"fileIdB64":               b64Std(checkpoint.FileID().Bytes()), "fileRevisionB64": b64Std(checkpoint.FileRevision().Bytes()),
					"canonicalPath": checkpoint.CanonicalPath(), "exactSize": "64", "backend": string(checkpoint.BackendID()),
					"rootIdentityB64": b64Std(checkpoint.RootIdentity().Bytes()), "ownedOutputObjectB64": b64Std(checkpoint.OwnedOutputObject().Bytes()),
					"stateGeneration": "1", "checkpointGeneration": "1",
					"verifiedRanges": []any{map[string]string{"start": "0", "end": "16"}, map[string]string{"start": "32", "end": "64"}},
					"phase":          uint8(checkpoint.Phase()), "commitState": uint8(checkpoint.CommitState()),
					"checksumB64":       b64Std(checkpoint.Checksum().Bytes()),
					"canonicalBytesB64": b64Std(checkpoint.CanonicalBytes()), "encodedB64": b64Std(checkpointEncoded),
				},
				map[string]any{
					"name":   "ownership",
					"marker": ownership.Marker, "namespace": ownership.Namespace, "backend": ownership.BackendID,
					"rootIdentityB64":   b64Std(ownership.RootIdentity.Bytes()),
					"canonicalBytesB64": b64Std(ownership.CanonicalBytes()), "encodedB64": b64Std(ownershipEncoded),
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

func transferIntentCase(name string, intent transfer.TransferIntent, share catalog.ShareInstance, root catalog.DirectoryID, selection map[string]any, target []byte, format string, backend transfer.OutputBackendID, jobID []byte) map[string]any {
	return map[string]any{
		"name": name, "shareInstanceB64": b64Std(share.Bytes()), "syntheticRootB64": b64Std(root.Bytes()),
		"selection":        selection,
		"output":           map[string]any{"targetKind": uint8(intent.OutputTarget().Kind()), "targetIdentityB64": b64Std(target), "backend": string(backend), "format": format},
		"transferJobIdB64": b64Std(jobID), "canonicalBytesB64": b64Std(intent.CanonicalBytes()), "digestB64": b64Std(intent.Digest().Bytes()),
	}
}

func directoryAdmissionCase(name string, admission transfer.DirectoryAdmission, secret []byte, id catalog.DirectoryID, generation catalog.DirectoryGeneration, path string, modified *catalog.ModifiedTime, parent []byte) map[string]any {
	return map[string]any{
		"name": name, "secretB64": b64Std(secret), "directoryIdB64": b64Std(id.Bytes()), "generationB64": b64Std(generation.Bytes()),
		"path": path, "parentTokenB64": nullableB64(parent), "modifiedTime": modifiedTimeJSON(modified),
		"tokenB64": b64Std(admission.Bytes()), "preimageB64": b64Std(directoryAdmissionPreimage(secret, id, generation, path, modified, parent)),
	}
}

func directoryAdmissionPreimage(secret []byte, id catalog.DirectoryID, generation catalog.DirectoryGeneration, path string, modified *catalog.ModifiedTime, parent []byte) []byte {
	result := make([]byte, 0, 128)
	result = append(result, []byte("windshare/directory-admission/session-v1\x00")...)
	result = append(result, secret...)
	result = append(result, id.Bytes()...)
	result = append(result, generation.Bytes()...)
	result = append(result, []byte(path)...)
	if modified == nil {
		result = append(result, 0)
		result = append(result, make([]byte, 8)...)
		result = append(result, make([]byte, 4)...)
		result = append(result, 0)
	} else {
		result = append(result, 1)
		var seconds [8]byte
		binary.BigEndian.PutUint64(seconds[:], uint64(modified.Seconds()))
		result = append(result, seconds[:]...)
		var nanoseconds [4]byte
		binary.BigEndian.PutUint32(nanoseconds[:], modified.Nanoseconds())
		result = append(result, nanoseconds[:]...)
		result = append(result, byte(modified.Precision()))
	}
	return append(result, parent...)
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
