package outputruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/transfer"
)

// These helpers deliberately assemble the smallest live authority needed by a
// test. Keeping the fixture at the public incremental boundary exercises the same
// root certification and namespace admission path as callers, while avoiding a
// second test-only implementation of the private session protocol.
func currentCoverageIntent(t *testing.T, root string, shareByte, rootByte byte) transfer.TransferIntent {
	t.Helper()
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewFilesystemTransferIntent(
		incrementalTestIdentity16[catalog.ShareInstance](shareByte),
		incrementalTestIdentity16[catalog.DirectoryID](rootByte),
		rules, root, filesystemOutputBackendID, transfer.OutputNativeTree,
	)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func currentCoverageSession(t *testing.T, rootSpec runtimeTestRootSpec, shareByte, rootByte byte) (*incrementalOutputSession, transfer.TransferIntent) {
	t.Helper()
	intent := currentCoverageIntent(t, rootSpec.path, shareByte, rootByte)
	authority := newIncrementalTestAuthority(t, rootSpec.path, openOutputRuntimeTestPlatform)
	opened, err := authority.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	session, ok := opened.(*incrementalOutputSession)
	if !ok {
		t.Fatalf("OpenOutput returned %T, want incremental session", opened)
	}
	return session, intent
}

func currentCoverageRoot(t *testing.T, session *incrementalOutputSession, intent transfer.TransferIntent, generationByte byte) transfer.DirectoryAdmission {
	t.Helper()
	admission, err := session.AdmitDirectory(context.Background(), transfer.OutputDirectory{
		DirectoryID: intent.SyntheticRoot(),
		Generation:  incrementalTestIdentity16[catalog.DirectoryGeneration](generationByte),
	})
	if err != nil {
		t.Fatal(err)
	}
	return admission
}

func currentCoverageFile(
	t *testing.T,
	session *incrementalOutputSession,
	intent transfer.TransferIntent,
	path string,
	parent transfer.DirectoryAdmission,
	fileByte, revisionByte byte,
	size uint64,
) transfer.OutputFile {
	t.Helper()
	geometry, err := content.NewFileGeometry(size, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(),
		incrementalTestIdentity16[catalog.FileID](fileByte),
		incrementalTestIdentity16[content.FileRevision](revisionByte),
		geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	target, err := outputTargetForDescriptor(session.SessionID(), descriptor, path)
	if err != nil {
		t.Fatal(err)
	}
	return transfer.OutputFile{
		Path: path, ExpectedSize: size, Descriptor: descriptor, Target: target,
		ParentAdmission: parent,
	}
}

func currentCoverageTransaction(
	t *testing.T,
	session *incrementalOutputSession,
	file transfer.OutputFile,
) *FileTransaction {
	t.Helper()
	start, err := session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	value, _, ok := start.Transaction()
	if !ok {
		t.Fatalf("BeginFile returned immediate settlement: %+v", start)
	}
	transaction, ok := value.(*FileTransaction)
	if !ok {
		t.Fatalf("BeginFile transaction type = %T", value)
	}
	return transaction
}

func TestCurrentOutputRuntimePublishesAndCleansIncrementalSession(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	session, intent := currentCoverageSession(t, rootSpec, 0x81, 0x82)
	rootAdmission := currentCoverageRoot(t, session, intent, 0x83)
	file := currentCoverageFile(t, session, intent, "published.bin", rootAdmission, 0x84, 0x85, 4)
	transaction := currentCoverageTransaction(t, session, file)
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Kind() != transfer.FilePublished {
		t.Fatalf("commit settlement = %v, want published", settlement.Kind())
	}
	if err := session.FinalizeDirectory(context.Background(), transfer.OutputDirectory{
		DirectoryID: intent.SyntheticRoot(),
		Generation:  incrementalTestIdentity16[catalog.DirectoryGeneration](0x83),
	}); err != nil {
		t.Fatal(err)
	}
	job, err := session.CompleteJob(context.Background(), transfer.JobSucceeded)
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind() != transfer.JobClosed {
		t.Fatalf("job settlement = %v, want closed", job.Kind())
	}
	if got, err := os.ReadFile(filepath.Join(rootSpec.path, "published.bin")); err != nil || string(got) != "data" {
		t.Fatalf("published output = %q, err=%v", got, err)
	}
}

func TestCurrentCompleteRemovesEmptyCheckpointNamespace(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	authority := newIncrementalTestAuthority(t, rootSpec.path, openOutputRuntimeTestPlatform)
	intent := currentCoverageIntent(t, rootSpec.path, 0xf9, 0xfa)
	opened, err := authority.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	session, ok := opened.(*incrementalOutputSession)
	if !ok {
		t.Fatalf("OpenOutput returned %T, want incremental session", opened)
	}
	_ = currentCoverageRoot(t, session, intent, 0xfb)
	if _, err := session.CompleteJob(context.Background(), transfer.JobSucceeded); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentOutputRuntimeSettlementBranches(t *testing.T) {
	t.Run("publish-collision", func(t *testing.T) {
		rootSpec := newRuntimeTestRootSpec(t)
		session, intent := currentCoverageSession(t, rootSpec, 0x91, 0x92)
		rootAdmission := currentCoverageRoot(t, session, intent, 0x93)
		file := currentCoverageFile(t, session, intent, "collision.bin", rootAdmission, 0x94, 0x95, 5)
		transaction := currentCoverageTransaction(t, session, file)
		if err := transaction.WriteRange(context.Background(), 0, []byte("owned")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootSpec.path, "collision.bin"), []byte("other"), 0o600); err != nil {
			t.Fatal(err)
		}
		settlement, err := transaction.Commit(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if settlement.Kind() != transfer.FilePublishBlocked {
			t.Fatalf("collision settlement = %v, want publish blocked", settlement.Kind())
		}
		_, _ = session.PauseJob(context.Background(), transfer.JobPauseSessionFailure)
	})

	t.Run("explicit-retirement", func(t *testing.T) {
		rootSpec := newRuntimeTestRootSpec(t)
		session, intent := currentCoverageSession(t, rootSpec, 0xa1, 0xa2)
		rootAdmission := currentCoverageRoot(t, session, intent, 0xa3)
		file := currentCoverageFile(t, session, intent, "retire.bin", rootAdmission, 0xa4, 0xa5, 4)
		transaction := currentCoverageTransaction(t, session, file)
		if _, err := transaction.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip); err != nil {
			t.Fatal(err)
		}
		_, _ = session.PauseJob(context.Background(), transfer.JobPauseInterrupted)
	})

	t.Run("pause-partial", func(t *testing.T) {
		rootSpec := newRuntimeTestRootSpec(t)
		session, intent := currentCoverageSession(t, rootSpec, 0xb1, 0xb2)
		rootAdmission := currentCoverageRoot(t, session, intent, 0xb3)
		file := currentCoverageFile(t, session, intent, "pause.bin", rootAdmission, 0xb4, 0xb5, 8)
		transaction := currentCoverageTransaction(t, session, file)
		if err := transaction.WriteRange(context.Background(), 0, []byte("part")); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
			t.Fatal(err)
		}
		_, _ = session.PauseJob(context.Background(), transfer.JobPauseInterrupted)
	})
}

func TestCurrentOutputRuntimeAdmissionAndPathBoundaries(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	session, intent := currentCoverageSession(t, rootSpec, 0xc1, 0xc2)
	rootAdmission := currentCoverageRoot(t, session, intent, 0xc3)
	if _, err := session.AdmitDirectory(context.Background(), transfer.OutputDirectory{
		DirectoryID: intent.SyntheticRoot(),
		Generation:  incrementalTestIdentity16[catalog.DirectoryGeneration](0xc3),
	}); err != nil {
		t.Fatal("idempotent root admission: ", err)
	}
	child := transfer.OutputDirectory{
		Path: "nested", DirectoryID: incrementalTestIdentity16[catalog.DirectoryID](0xc4),
		Generation:      incrementalTestIdentity16[catalog.DirectoryGeneration](0xc5),
		ParentAdmission: rootAdmission,
	}
	childAdmission, err := session.AdmitDirectory(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.AdmitDirectory(context.Background(), transfer.OutputDirectory{
		Path: "nested", DirectoryID: child.DirectoryID,
		Generation:      incrementalTestIdentity16[catalog.DirectoryGeneration](0xc6),
		ParentAdmission: rootAdmission,
	}); !errors.Is(err, transfer.ErrDirectoryAdmissionMismatch) {
		t.Fatalf("changed child generation error = %v", err)
	}
	if _, err := session.BeginFile(context.Background(), currentCoverageFile(t, session, intent, "nested/child.bin", childAdmission, 0xc7, 0xc8, 2)); err != nil {
		t.Fatal("child file admission: ", err)
	}
	_, _ = session.PauseJob(context.Background(), transfer.JobPauseInterrupted)
}

func TestCurrentOutputRuntimeInvalidAuthorityInputs(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	authority := newIncrementalTestAuthority(t, rootSpec.path, openOutputRuntimeTestPlatform)
	if _, err := authority.OpenOutput(context.Background(), transfer.TransferIntent{}); !errors.Is(err, transfer.ErrInvalidTransferIntent) {
		t.Fatalf("zero intent error = %v", err)
	}
}

func TestCurrentOutputSelectionPreflightAndMaterialization(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	platform, err := openOutputRuntimeTestPlatform(rootSpec.path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = platform.Close() })
	root := incrementalTestIdentity16[catalog.DirectoryID](0xd1)
	rootGeneration := incrementalTestIdentity16[catalog.DirectoryGeneration](0xd2)
	share := incrementalTestIdentity16[catalog.ShareInstance](0xd3)
	directoryID := incrementalTestIdentity16[catalog.DirectoryID](0xd4)
	directoryGeneration := incrementalTestIdentity16[catalog.DirectoryGeneration](0xd5)
	fileID := incrementalTestIdentity16[catalog.FileID](0xd6)
	modified, err := catalog.NewModifiedTime(4, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewOutputSelection(share, root, rootGeneration,
		[]transfer.OutputSelectionDirectory{{
			Path: "nested", DirectoryID: directoryID, Generation: directoryGeneration, ModifiedTime: modified,
		}},
		[]transfer.OutputSelectionFile{{
			Path: "nested/file.bin", FileID: fileID, ParentDirectoryID: directoryID,
			ParentGeneration: directoryGeneration, ExpectedSize: 3, ModifiedTime: modified,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	intent := currentCoverageIntent(t, rootSpec.path, 0xd7, 0xd8)
	if _, err := preflightOutputSelectionAdmissionWithIntent(platform, selection, intent.Digest()); err != nil {
		t.Fatalf("selection admission: %v", err)
	}
	if err := validateReservedOutputSelection(platform, selection); err != nil {
		t.Fatalf("reserved-path validation: %v", err)
	}
	if err := preflightOutputSelectionParents(platform, selection); err != nil {
		t.Fatalf("selection parent preflight: %v", err)
	}
	if err := preflightOutputSelectionAuthorities(platform, selection); err != nil {
		t.Fatalf("selection authority preflight: %v", err)
	}
	if err := materializeOutputSelection(platform.Root(), selection); err != nil {
		t.Fatalf("selection materialization: %v", err)
	}
	if _, err := openOutputDirectoryPath(platform.Root(), "nested", false); err != nil {
		t.Fatalf("materialized directory: %v", err)
	}

	reservedSelection, err := transfer.NewOutputSelection(share, root, rootGeneration, nil,
		[]transfer.OutputSelectionFile{{
			Path: ".windshare-outputish", FileID: incrementalTestIdentity16[catalog.FileID](0xd9),
			ParentDirectoryID: root, ParentGeneration: rootGeneration, ExpectedSize: 1,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateReservedOutputSelection(platform, reservedSelection); !errors.Is(err, outputfault.ErrReservedPath) {
		t.Fatalf("reserved selection error = %v", err)
	}
	if _, err := preflightOutputSelectionAdmissionWithIntent(platform, transfer.OutputSelection{}, intent.Digest()); !errors.Is(err, transfer.ErrInvalidOutputSelection) {
		t.Fatalf("zero selection error = %v", err)
	}
	if _, err := preflightOutputSelectionAdmissionWithIntent(platform, selection, transfer.TransferIntentDigest{}); !errors.Is(err, transfer.ErrInvalidOutputSelection) {
		t.Fatalf("zero intent digest error = %v", err)
	}
}
