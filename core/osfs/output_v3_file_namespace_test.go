package osfs

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestScanOutputV3FileNamespaceBindsRecordsAndGroupsDuplicateObjects(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelectionPaths(t, []string{"first.bin", "second.bin"}, 10)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	session := opened.Session
	t.Cleanup(func() { v3RecoveryCloseSession(t, session) })

	object, err := resumestate.OutputObjectIDFromBytes(bytes.Repeat([]byte{9}, resumestate.OutputObjectIDBytes))
	if err != nil {
		t.Fatal(err)
	}
	for index, selected := range selection.Files() {
		outputFile := v3RecoveryOutputFileAt(t, session, selection, index)
		authority, createErr := resumestate.NewFileRecord(resumestate.FileRecordSpec{
			Session: session.state, Descriptor: outputFile.Descriptor,
			CanonicalLocator: selected.Path, OutputObject: object,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		installFileNamespaceTestRecord(t, session, authority.Bound())
	}
	invalidShard, err := session.filesDir.CreateFile("zz", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(invalidShard.Close(), session.filesDir.Sync()); err != nil {
		t.Fatal(err)
	}

	snapshot, err := scanOutputV3FileNamespace(session)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.records) != 2 || len(snapshot.duplicateObjects) != 1 ||
		len(snapshot.duplicateObjects[0].records) != 2 ||
		snapshot.duplicateObjects[0].object != object {
		t.Fatalf("snapshot records=%d duplicate groups=%+v", len(snapshot.records), snapshot.duplicateObjects)
	}
	codes := make(map[string]int)
	for _, attention := range snapshot.attention {
		codes[attention.Code]++
	}
	if codes["duplicate-output-object"] != 2 || codes["unclassified-file-shard"] != 1 {
		t.Fatalf("attention codes = %v", codes)
	}
	decision, err := resumestate.ReduceDuplicateOutputObject(
		snapshot.duplicateObjects[0].records[0].bound,
		snapshot.duplicateObjects[0].records[1].bound,
	)
	if err != nil || decision.QuarantineReason() != resumestate.QuarantineOutputObjectDuplicate {
		t.Fatalf("duplicate decision = %+v, %v", decision, err)
	}
}

func TestScanOutputV3FileNamespaceRejectsEntryBeforeDecodingBeyondSelectionBudget(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	session := opened.Session
	t.Cleanup(func() { v3RecoveryCloseSession(t, session) })

	name := resumestate.FileRecordName(resumestate.DigestCanonicalLocator("unselected.bin"))
	shard, present, err := openOutputShard(session.filesDir, name.Shard(), true)
	if err != nil || !present {
		t.Fatalf("create shard = %v, %v", present, err)
	}
	record, err := shard.CreateFile(name.Name(), true, 0)
	if err != nil {
		_ = shard.Close()
		t.Fatal(err)
	}
	if err := errors.Join(record.Close(), shard.Sync(), shard.Close()); err != nil {
		t.Fatal(err)
	}

	if _, err := scanOutputV3FileNamespace(session); !errors.Is(err, resumestate.ErrFileStateNamespaceLimit) {
		t.Fatalf("namespace budget error = %v", err)
	}
}

func TestOpenOutputV3SessionDurablyQuarantinesEveryDuplicateObjectClaimBeforeContent(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelectionPaths(t, []string{"first.bin", "second.bin"}, 10)
	firstOpen := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	object, err := resumestate.OutputObjectIDFromBytes(bytes.Repeat([]byte{9}, resumestate.OutputObjectIDBytes))
	if err != nil {
		t.Fatal(err)
	}
	for index, selected := range selection.Files() {
		outputFile := v3RecoveryOutputFileAt(t, firstOpen.Session, selection, index)
		authority, createErr := resumestate.NewFileRecord(resumestate.FileRecordSpec{
			Session: firstOpen.Session.state, Descriptor: outputFile.Descriptor,
			CanonicalLocator: selected.Path, OutputObject: object,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		installFileNamespaceTestRecord(t, firstOpen.Session, authority.Bound())
	}
	v3RecoveryCloseSession(t, firstOpen.Session)

	reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, reopened.Session) })
	duplicateAttention := 0
	for _, attention := range reopened.Session.attention {
		if attention.Code == "duplicate-output-object" {
			duplicateAttention++
		}
	}
	if duplicateAttention != len(selection.Files()) {
		t.Fatalf("duplicate attention = %d, want %d", duplicateAttention, len(selection.Files()))
	}

	snapshot, err := scanOutputV3FileNamespace(reopened.Session)
	if err != nil || len(snapshot.records) != len(selection.Files()) {
		t.Fatalf("post-quarantine snapshot records = %d, %v", len(snapshot.records), err)
	}
	for index, scanned := range snapshot.records {
		if scanned.bound.Record().Phase() != resumestate.FileQuarantined ||
			scanned.bound.Record().QuarantineReason() != resumestate.QuarantineOutputObjectDuplicate {
			t.Fatalf("record %d was not durably duplicate-quarantined: %+v", index, scanned.bound.Record())
		}
		start, beginErr := reopened.Session.BeginFile(
			context.Background(), v3RecoveryOutputFileAt(t, reopened.Session, selection, index),
		)
		settlement, settled := start.ImmediateSettlement()
		if beginErr != nil || !settled || settlement.Kind() != transfer.FileQuarantined {
			t.Fatalf("record %d content gate = %+v, settled=%v, err=%v", index, settlement, settled, beginErr)
		}
	}
}

func installFileNamespaceTestRecord(
	t *testing.T,
	session *filesystemOutputSession,
	bound resumestate.BoundFileRecord,
) {
	t.Helper()
	name := resumestate.FileRecordName(bound.Record().LocatorDigest())
	shard, present, err := openOutputShard(session.filesDir, name.Shard(), true)
	if err != nil || !present {
		t.Fatalf("open file-state shard = %v, %v", present, err)
	}
	encoded, err := resumestate.EncodeFileRecord(bound)
	if err == nil {
		_, err = session.store.ensureInitialRecord(shard, name.Name(), encoded, resumestate.MaxFileStateBytes)
	}
	if err := errors.Join(err, shard.Close()); err != nil {
		t.Fatal(err)
	}
}
