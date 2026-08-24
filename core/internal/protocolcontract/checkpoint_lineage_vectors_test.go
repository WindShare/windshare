package protocolcontract

import (
	"crypto/sha256"
	"strconv"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func newFileCheckpointVectorRecord(
	t *testing.T,
	name string,
	base osfs.FileCheckpointSpec,
	mutate func(*osfs.FileCheckpointSpec),
) osfs.FileCheckpointV2 {
	t.Helper()
	spec := base
	spec.AuthorityRef = append([]byte(nil), base.AuthorityRef...)
	spec.OwnedObjectID = append([]byte(nil), base.OwnedObjectID...)
	spec.VerifiedRanges = append([]osfs.FileCheckpointRange(nil), base.VerifiedRanges...)
	if mutate != nil {
		mutate(&spec)
	}
	checkpoint, err := osfs.NewFileCheckpointV2(spec)
	if err != nil {
		t.Fatalf("%s checkpoint: %v", name, err)
	}
	return checkpoint
}

func buildFileCheckpointLineageVectorCases(
	t *testing.T,
	receiveIntentCase string,
	base osfs.FileCheckpointSpec,
) []any {
	t.Helper()
	result := make([]any, 0, 15)
	appendVariant := func(
		name string,
		axis string,
		relation string,
		mutate func(*osfs.FileCheckpointSpec),
	) {
		checkpoint := newFileCheckpointVectorRecord(t, name, base, mutate)
		vector := fileCheckpointVectorCase(t, name, receiveIntentCase, checkpoint)
		vector["lineageAxis"] = axis
		vector["lineageRelation"] = relation
		result = append(result, vector)
	}

	changedRevision, err := content.FileRevisionFromBytes(contractSequence(0x32, content.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	changedOperation, err := receivecontract.OperationIDFromBytes(
		contractSequence(0x12, receivecontract.StableIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	changedIntent, err := transfer.ReceiveIntentDigestFromBytes(
		contractSequence(0x13, transfer.ReceiveIntentDigestBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	changedBinding, err := receivecontract.BindingDigestFromBytes(contractSequence(0x14, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	changedFile := mustFile(t, contractSequence(0x15, catalog.IdentityBytes))

	appendVariant("lineage-excludes-revision", "file-revision", "same", func(spec *osfs.FileCheckpointSpec) {
		spec.FileRevision = changedRevision
	})
	appendVariant("lineage-excludes-size", "exact-size", "same", func(spec *osfs.FileCheckpointSpec) {
		spec.ExactSize = 96
	})
	appendVariant("lineage-excludes-owned-object", "owned-object", "same", func(spec *osfs.FileCheckpointSpec) {
		spec.OwnedObjectID = contractSequence(0x52, sha256.Size)
	})
	appendVariant("lineage-operation", "operation", "different", func(spec *osfs.FileCheckpointSpec) {
		spec.OperationID = changedOperation
	})
	appendVariant("lineage-intent", "receive-intent", "different", func(spec *osfs.FileCheckpointSpec) {
		spec.ReceiveIntentDigest = changedIntent
	})
	appendVariant("lineage-binding", "materialization-binding", "different", func(spec *osfs.FileCheckpointSpec) {
		spec.MaterializationBindingDigest = changedBinding
	})
	appendVariant("lineage-file", "file", "different", func(spec *osfs.FileCheckpointSpec) {
		spec.FileID = changedFile
	})
	appendVariant("lineage-path-segments-a", "canonical-path", "different", func(spec *osfs.FileCheckpointSpec) {
		spec.CanonicalPath = "a/bc"
	})
	appendVariant("lineage-path-segments-b", "canonical-path", "different", func(spec *osfs.FileCheckpointSpec) {
		spec.CanonicalPath = "ab/c"
	})
	appendVariant("lineage-path-unicode", "canonical-path", "different", func(spec *osfs.FileCheckpointSpec) {
		spec.CanonicalPath = "资料/café.bin"
	})
	appendVariant("lineage-materializer-legacy-fsa", "materializer", "different", func(spec *osfs.FileCheckpointSpec) {
		spec.MaterializerKind = osfs.FileCheckpointMaterializerLegacyFSATree
	})
	appendVariant("lineage-materializer-origin-private", "materializer", "different", func(spec *osfs.FileCheckpointSpec) {
		spec.MaterializerKind = osfs.FileCheckpointMaterializerOriginPrivate
	})
	appendVariant("lineage-materializer-atomic", "materializer", "different", func(spec *osfs.FileCheckpointSpec) {
		spec.MaterializerKind = osfs.FileCheckpointMaterializerAtomicFile
	})
	appendVariant("lineage-materializer-fsa", "materializer", "different", func(spec *osfs.FileCheckpointSpec) {
		spec.MaterializerKind = osfs.FileCheckpointMaterializerFSATree
	})
	appendVariant("lineage-materializer-fsa-root-file", "canonical-path", "different", func(spec *osfs.FileCheckpointSpec) {
		spec.MaterializerKind = osfs.FileCheckpointMaterializerFSATree
		spec.CanonicalPath = ""
	})
	return result
}

func fileCheckpointVectorCase(
	t *testing.T,
	name string,
	receiveIntentCase string,
	checkpoint osfs.FileCheckpointV2,
) map[string]any {
	t.Helper()
	encoded, err := osfs.EncodeFileCheckpointV2(checkpoint)
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
	lineageID, err := checkpoint.CheckpointLineageID()
	if err != nil {
		t.Fatal(err)
	}
	lineageCanonical, err := checkpoint.CheckpointLineageCanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"name": name, "receiveIntentCase": receiveIntentCase,
		"schemaVersion":   checkpoint.SchemaVersion(),
		"ownershipMarker": checkpoint.OwnershipMarker(), "namespace": checkpoint.Namespace(),
		"recordId":                              b64URL(checkpoint.RecordID().Bytes()),
		"checkpointLineageId":                   b64URL(lineageID.Bytes()),
		"checkpointLineageCanonicalBytesB64Url": b64URL(lineageCanonical),
		"operationId":                           b64URL(checkpoint.OperationID().Bytes()),
		"receiveIntentDigest":                   b64URL(checkpoint.ReceiveIntentDigest().Bytes()),
		"materializationBindingDigest":          b64URL(checkpoint.MaterializationBindingDigest().Bytes()),
		"fileId":                                b64URL(checkpoint.FileID().Bytes()),
		"fileRevision":                          b64URL(checkpoint.FileRevision().Bytes()),
		"canonicalPath":                         canonicalPathSegments(checkpoint.CanonicalPath()),
		"exactSize":                             strconv.FormatUint(checkpoint.ExactSize(), 10),
		"materializerKind":                      uint8(checkpoint.MaterializerKind()),
		"authorityRef":                          b64URL(checkpoint.AuthorityRef().Bytes()),
		"ownedObjectId":                         b64URL(checkpoint.OwnedObjectID().Bytes()),
		"stateGeneration":                       strconv.FormatUint(checkpoint.StateGeneration(), 10),
		"checkpointGeneration":                  strconv.FormatUint(checkpoint.CheckpointGeneration(), 10),
		"verifiedRanges":                        ranges,
		"phase":                                 uint8(checkpoint.Phase()), "commitState": uint8(checkpoint.CommitState()),
		"quarantineReason":     uint8(checkpoint.QuarantineReason()),
		"quarantineOrigin":     uint8(checkpoint.QuarantineOrigin()),
		"retirementReason":     uint8(checkpoint.RetirementReason()),
		"checksum":             b64URL(checkpoint.Checksum().Bytes()),
		"canonicalBytesB64Url": b64URL(checkpoint.CanonicalBytes()),
		"encodedB64Url":        b64URL(encoded),
	}
}
