package directoryauthority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer"
)

const publicationFixtureSize = 64

func TestPublicationObserverPinsExactFinalIdentityAndDetectsReplacement(t *testing.T) {
	platform := newFakePlatform(outputcap.CallerProvidedContainer)
	folder := platform.addDirectory(platform.rootNode(), "folder")
	final := platform.addFile(folder, "final.bin", publicationFixtureSize)
	observer, err := NewPublicationObserver(platform)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := publicationCheckpoint{
		record: publicationRecord(t, "folder/final.bin", 0x61), owned: final,
	}
	pin, err := observer.PinPublication(context.Background(), checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Observation().FinalEvidence() != resumeauthority.EvidenceExact {
		t.Fatalf("initial evidence = %v", pin.Observation().FinalEvidence())
	}
	if evidence, err := pin.Revalidate(context.Background()); err != nil ||
		evidence != resumeauthority.EvidenceExact {
		t.Fatalf("exact revalidation = %v, %v", evidence, err)
	}
	platform.addFile(folder, "final.bin", publicationFixtureSize)
	if evidence, err := pin.Revalidate(context.Background()); err != nil ||
		evidence != resumeauthority.EvidenceReplaced {
		t.Fatalf("replacement revalidation = %v, %v", evidence, err)
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := pin.Revalidate(context.Background()); !errors.Is(err, ErrAuthorityClosed) {
		t.Fatalf("closed pin error = %v", err)
	}
}

func TestPublicationObserverRetainsAbsentCutsAndFailsClosedOnCreation(t *testing.T) {
	platform := newFakePlatform(outputcap.CallerProvidedContainer)
	observer, err := NewPublicationObserver(platform)
	if err != nil {
		t.Fatal(err)
	}
	record := publicationRecord(t, "missing/final.bin", 0x62)
	pin, err := observer.PinPublication(context.Background(), publicationCheckpoint{record: record})
	if err != nil {
		t.Fatal(err)
	}
	if pin.Observation().FinalEvidence() != resumeauthority.EvidenceAbsent {
		t.Fatalf("missing-parent evidence = %v", pin.Observation().FinalEvidence())
	}
	if evidence, err := pin.Revalidate(context.Background()); err != nil ||
		evidence != resumeauthority.EvidenceAbsent {
		t.Fatalf("stable absence = %v, %v", evidence, err)
	}
	platform.addDirectory(platform.rootNode(), "missing")
	if evidence, err := pin.Revalidate(context.Background()); err != nil ||
		evidence != resumeauthority.EvidenceAmbiguous {
		t.Fatalf("created ancestor = %v, %v", evidence, err)
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}

	folder := platform.addDirectory(platform.rootNode(), "folder")
	record = publicationRecord(t, "folder/final.bin", 0x63)
	pin, err = observer.PinPublication(context.Background(), publicationCheckpoint{record: record})
	if err != nil {
		t.Fatal(err)
	}
	if pin.Observation().FinalEvidence() != resumeauthority.EvidenceAbsent {
		t.Fatalf("missing-final evidence = %v", pin.Observation().FinalEvidence())
	}
	platform.addFile(folder, "final.bin", publicationFixtureSize)
	if evidence, err := pin.Revalidate(context.Background()); err != nil ||
		evidence != resumeauthority.EvidenceReplaced {
		t.Fatalf("created final = %v, %v", evidence, err)
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationObserverTreatsAliasWrongKindAndChangedLineageAsAmbiguous(t *testing.T) {
	tests := []struct {
		name  string
		build func(*fakePlatform) publicationCheckpoint
	}{
		{
			name: "alias ancestor",
			build: func(platform *fakePlatform) publicationCheckpoint {
				platform.addDirectory(platform.rootNode(), "Folder")
				return publicationCheckpoint{record: publicationRecord(t, "folder/final.bin", 0x64)}
			},
		},
		{
			name: "directory at final",
			build: func(platform *fakePlatform) publicationCheckpoint {
				folder := platform.addDirectory(platform.rootNode(), "folder")
				platform.addDirectory(folder, "final.bin")
				return publicationCheckpoint{record: publicationRecord(t, "folder/final.bin", 0x65)}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			platform := newFakePlatform(outputcap.CallerProvidedContainer)
			observer, err := NewPublicationObserver(platform)
			if err != nil {
				t.Fatal(err)
			}
			pin, err := observer.PinPublication(context.Background(), test.build(platform))
			if err != nil {
				t.Fatal(err)
			}
			if pin.Observation().FinalEvidence() != resumeauthority.EvidenceAmbiguous {
				t.Fatalf("initial evidence = %v", pin.Observation().FinalEvidence())
			}
			if err := pin.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}

	platform := newFakePlatform(outputcap.CallerProvidedContainer)
	folder := platform.addDirectory(platform.rootNode(), "folder")
	sub := platform.addDirectory(folder, "sub")
	final := platform.addFile(sub, "final.bin", publicationFixtureSize)
	observer, err := NewPublicationObserver(platform)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := observer.PinPublication(context.Background(), publicationCheckpoint{
		record: publicationRecord(t, "folder/sub/final.bin", 0x66), owned: final,
	})
	if err != nil {
		t.Fatal(err)
	}
	platform.replaceDirectory(platform.rootNode(), "folder")
	if evidence, err := pin.Revalidate(context.Background()); err != nil ||
		evidence != resumeauthority.EvidenceAmbiguous {
		t.Fatalf("changed lineage = %v, %v", evidence, err)
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationObserverRejectsReservedControlPath(t *testing.T) {
	platform := newFakePlatform(outputcap.CallerProvidedContainer)
	observer, err := NewPublicationObserver(platform)
	if err != nil {
		t.Fatal(err)
	}
	_, err = observer.PinPublication(context.Background(), publicationCheckpoint{
		record: publicationRecord(t, ".windshare-output/final.bin", 0x67),
	})
	if !errors.Is(err, ErrInvalidLocator) {
		t.Fatalf("reserved path error = %v", err)
	}
}

func TestPublicationObserverTurnsObservationRaceIntoTypedAmbiguity(t *testing.T) {
	platform := newFakePlatform(outputcap.CallerProvidedContainer)
	platform.addFile(platform.rootNode(), "final.bin", publicationFixtureSize)
	platform.setOpenEntryHook(func() {
		platform.mu.Lock()
		delete(platform.root.entries, "final.bin")
		platform.mu.Unlock()
	})
	observer, err := NewPublicationObserver(platform)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := observer.PinPublication(context.Background(), publicationCheckpoint{
		record: publicationRecord(t, "final.bin", 0x68),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pin.Observation().FinalEvidence() != resumeauthority.EvidenceAmbiguous {
		t.Fatalf("raced observation evidence = %v", pin.Observation().FinalEvidence())
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationObserverNeverProjectsPresentFileAsAbsent(t *testing.T) {
	platform := newFakePlatform(outputcap.CallerProvidedContainer)
	final := platform.addFile(platform.rootNode(), "final.bin", publicationFixtureSize)
	observer, err := NewPublicationObserver(platform)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := observer.PinPublication(context.Background(), publicationCheckpoint{
		record:   publicationRecord(t, "final.bin", 0x69),
		owned:    final,
		evidence: resumeauthority.EvidenceAbsent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pin.Observation().FinalEvidence() != resumeauthority.EvidenceAmbiguous {
		t.Fatalf("present-file evidence = %v", pin.Observation().FinalEvidence())
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
}

type publicationCheckpoint struct {
	record   checkpointmodel.Record
	owned    *fakeNode
	evidence resumeauthority.Evidence
}

func (checkpoint publicationCheckpoint) Record() checkpointmodel.Record { return checkpoint.record }

func (checkpoint publicationCheckpoint) SameOwnedFile(
	ctx context.Context,
	file outputcap.File,
) (resumeauthority.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return resumeauthority.EvidenceAmbiguous, err
	}
	if checkpoint.evidence.Valid() {
		return checkpoint.evidence, nil
	}
	public, ok := file.(*fakeFile)
	if !ok || public == nil || public.node == nil {
		return resumeauthority.EvidenceAmbiguous, errFakeUnsupported
	}
	if checkpoint.owned != nil && public.node == checkpoint.owned {
		return resumeauthority.EvidenceExact, nil
	}
	return resumeauthority.EvidenceReplaced, nil
}

func publicationRecord(t *testing.T, path string, seed byte) checkpointmodel.Record {
	t.Helper()
	intent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0x41}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	var fileID catalog.FileID
	var revision content.FileRevision
	for index := range fileID {
		fileID[index] = seed + byte(index)
		revision[index] = seed + byte(index) + 1
	}
	record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		TransferIntentDigest: intent,
		FileID:               fileID,
		FileRevision:         revision,
		CanonicalPath:        path,
		ExactSize:            publicationFixtureSize,
		BackendID:            "publication-test",
		RootIdentity:         bytes.Repeat([]byte{0x51}, sha256.Size),
		OwnedOutputObject:    bytes.Repeat([]byte{seed}, sha256.Size),
		StateGeneration:      2,
		CheckpointGeneration: 1,
		VerifiedRanges:       []checkpointmodel.Range{{Offset: 0, End: 16}},
		Phase:                checkpointmodel.PhasePublished,
		CommitState:          checkpointmodel.CommitPublished,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

var _ resumeauthority.PinnedCheckpoint = publicationCheckpoint{}
