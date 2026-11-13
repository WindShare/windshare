package checkpointmodel

import (
	"bytes"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func TestMetadataAdmissionPreservesOneObjectAndExplicitRevisionEvidence(t *testing.T) {
	spec := canonicalRecordSpec(t)
	spec.SourceMetadata = content.SourceMetadata{1}
	spec.CommitState = CommitVerified
	previous := mustCanonicalRecord(t, spec)
	geometry, _ := content.NewFileGeometry(previous.ExactSize(), catalog.DefaultChunkSize)
	revision := previous.FileRevision()
	revision[0]++
	current, _ := content.NewFileRevisionDescriptor(catalog.ShareInstance{1}, previous.FileID(), revision, geometry, catalog.ModifiedTime{})
	current = content.BindRevisionMetadata(current, spec.SourceMetadata)
	next, err := AdmitMetadataReuse(previous, current)
	if err != nil {
		t.Fatal(err)
	}
	if next.RecordID() != previous.RecordID() || next.OwnedObjectID() != previous.OwnedObjectID() ||
		next.FileRevision() != current.FileRevision() || next.ReusedFrom() != previous.FileRevision() ||
		next.StateGeneration() != previous.StateGeneration()+1 || next.CheckpointGeneration() != previous.CheckpointGeneration()+1 {
		t.Fatal("metadata admission lost the prior authority or current revision")
	}
	encoded, err := EncodeRecord(next)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeRecord(encoded)
	if err != nil || !bytes.Equal(restored.CanonicalBytes(), next.CanonicalBytes()) ||
		ValidateTransition(previous, restored) != nil {
		t.Fatalf("admission did not survive the durable codec: %v", err)
	}
	if ValidateTransition(next, previous) == nil {
		t.Fatal("stale predecessor could replace accepted progress")
	}
	for _, commit := range []CommitState{CommitCandidate, CommitPublished} {
		testSpec := spec
		testSpec.CommitState = commit
		if commit == CommitPublished {
			testSpec.Phase = PhasePublished
			testSpec.VerifiedRanges = []Range{{Offset: 0, End: spec.ExactSize}}
		}
		record := mustCanonicalRecord(t, testSpec)
		if _, err := AdmitMetadataReuse(record, current); err == nil {
			t.Fatal("uncommitted or historical data was rebound")
		}
	}
	for name, metadata := range map[string]content.SourceMetadata{"missing": {}, "changed": {2}} {
		t.Run(name, func(t *testing.T) {
			if _, err := AdmitMetadataReuse(previous, content.BindRevisionMetadata(current, metadata)); err == nil {
				t.Fatal("insufficient metadata authorized reuse")
			}
		})
	}
	differentObject := next
	differentObject.ownedObjectID[0]++
	differentObject.checksum = differentObject.derivedChecksum()
	if ValidateTransition(previous, differentObject) == nil {
		t.Fatal("comparison granted authority over another output object")
	}
}
