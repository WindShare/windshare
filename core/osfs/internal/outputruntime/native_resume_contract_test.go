package outputruntime

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestNativeResumeRepositoryRetainsCorruptLifecycleAsObservationOnly(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	intent := nativeTestIntent(t, root, 0xd1, 0xd2)
	operationName := hex.EncodeToString(intent.OperationID().Bytes())
	lifecyclePath := filepath.Join(
		root,
		checkpointstore.ControlDirectory,
		checkpointstore.CheckpointDirectory,
		checkpointstore.OperationsDirectory,
		operationName,
		checkpointstore.ReceiptsDirectory,
		"lifecycle",
	)
	corruptLifecycle := []byte("corrupt lifecycle")
	if err := os.WriteFile(lifecyclePath, corruptLifecycle, 0o600); err != nil {
		t.Fatal(err)
	}

	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := repository.List(context.Background())
	if err != nil || len(snapshots) != 1 ||
		!bytes.Equal(snapshots[0].LifecycleRecord, corruptLifecycle) {
		t.Fatalf("corrupt lifecycle snapshot = (%+v, %v)", snapshots, err)
	}
	operation, err := checkpointmodel.DecodeReceiveOperation(snapshots[0].OperationRecord)
	if err != nil || operation.OperationID() != intent.OperationID() {
		t.Fatalf("corrupt lifecycle operation = (%v, %v)", operation.OperationID(), err)
	}
}

func TestNativeResumeRepositoryRejectsAmbiguousOperationInventory(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	_ = nativeTestIntent(t, root, 0xd3, 0xd4)
	operationsPath := filepath.Join(
		root,
		checkpointstore.ControlDirectory,
		checkpointstore.CheckpointDirectory,
		checkpointstore.OperationsDirectory,
	)
	if err := os.Mkdir(filepath.Join(operationsPath, "AMBIGUOUS"), 0o700); err != nil {
		t.Fatal(err)
	}
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	if snapshots, err := repository.List(context.Background()); snapshots != nil ||
		!errors.Is(err, ErrNativeResumeOwnershipUnknown) {
		t.Fatalf("ambiguous operation inventory = (%+v, %v)", snapshots, err)
	}
}

func TestNativeResumeRepositoryRejectsCorruptImmutableOperation(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	intent := nativeTestIntent(t, root, 0xd8, 0xd9)
	operationPath := filepath.Join(
		root,
		checkpointstore.ControlDirectory,
		checkpointstore.CheckpointDirectory,
		checkpointstore.OperationsDirectory,
		hex.EncodeToString(intent.OperationID().Bytes()),
		checkpointstore.OperationFile,
	)
	corruptOperation := []byte("corrupt operation")
	if err := os.WriteFile(operationPath, corruptOperation, 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	if snapshots, err := repository.List(context.Background()); snapshots != nil || err == nil {
		t.Fatalf("corrupt operation inventory = (%+v, %v)", snapshots, err)
	}
	if after, err := os.ReadFile(operationPath); err != nil || !bytes.Equal(after, corruptOperation) {
		t.Fatalf("corrupt immutable operation was mutated = (%q, %v)", after, err)
	}
}

func TestNativeResumeRepositoryProjectsMissingRepositoryStructureAsUnknown(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	intent := nativeTestIntent(t, root, 0xda, 0xdb)
	operationName := hex.EncodeToString(intent.OperationID().Bytes())
	manifestsPath := filepath.Join(
		root,
		checkpointstore.ControlDirectory,
		checkpointstore.CheckpointDirectory,
		checkpointstore.OperationsDirectory,
		operationName,
		checkpointstore.ManifestsDirectory,
	)
	if err := os.Remove(manifestsPath); err != nil {
		t.Fatal(err)
	}
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := repository.List(context.Background())
	if err != nil || len(snapshots) != 1 || !bytes.Equal(snapshots[0].LifecycleRecord, []byte{0}) {
		t.Fatalf("missing repository structure snapshot = (%+v, %v)", snapshots, err)
	}
	operation, err := checkpointmodel.DecodeReceiveOperation(snapshots[0].OperationRecord)
	if err != nil || operation.OperationID() != intent.OperationID() {
		t.Fatalf("missing structure operation = (%v, %v)", operation.OperationID(), err)
	}
	if _, err := repository.Acquire(context.Background(), intent.OperationID()); err == nil {
		t.Fatal("missing repository structure unexpectedly yielded mutation authority")
	}
}

func TestNativeResumeLeaseRevalidatesImmutableOperationOnEverySnapshot(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	intent := nativeTestIntent(t, root, 0xdc, 0xdd)
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := repository.Acquire(context.Background(), intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	operationPath := filepath.Join(
		root,
		checkpointstore.ControlDirectory,
		checkpointstore.CheckpointDirectory,
		checkpointstore.OperationsDirectory,
		hex.EncodeToString(intent.OperationID().Bytes()),
		checkpointstore.OperationFile,
	)
	if err := os.WriteFile(operationPath, []byte("replaced operation"), 0o600); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := lease.Snapshot(context.Background()); len(snapshot.OperationRecord) != 0 || err == nil {
		t.Fatalf("replaced operation snapshot = (%+v, %v)", snapshot, err)
	}
}

func TestNativeResumeRepositoryAndLeaseRejectIncompleteAuthority(t *testing.T) {
	ctx := context.Background()
	operation := incrementalTestIdentity16[receivecontract.OperationID](0xd5)
	var missing *NativeResumeRepository
	if snapshots, err := missing.List(ctx); snapshots != nil || !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil repository list = (%+v, %v)", snapshots, err)
	}
	if lease, err := missing.Acquire(ctx, operation); lease != nil || !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil repository acquire = (%v, %v)", lease, err)
	}
	uncleanRoot := filepath.Join(t.TempDir(), "child") + string(filepath.Separator) + ".."
	for _, root := range []string{"", "relative", uncleanRoot} {
		if repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform); repository != nil ||
			!errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("invalid root %q = (%v, %v)", root, repository, err)
		}
	}
	if repository, err := NewNativeResumeRepository(filepath.Clean(t.TempDir()), nil); repository != nil ||
		!errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil platform factory = (%v, %v)", repository, err)
	}

	root := newRuntimeTestRootSpec(t).path
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repository.List(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled list error = %v", err)
	}
	if _, err := repository.Acquire(canceled, operation); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire error = %v", err)
	}

	lease := &NativeResumeLease{}
	if _, err := lease.Snapshot(ctx); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero lease snapshot error = %v", err)
	}
	if _, err := lease.ObserveRecovery(ctx); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero lease recovery error = %v", err)
	}
	if _, err := lease.CleanupOwned(ctx); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero lease cleanup error = %v", err)
	}
	if err := lease.InstallReceipt(ctx, nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero lease receipt error = %v", err)
	}
	if err := lease.ReplaceLifecycle(ctx, nil, nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero lease lifecycle error = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("zero lease close error = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("idempotent zero lease close error = %v", err)
	}
	var nilLease *NativeResumeLease
	if err := nilLease.Close(); err != nil {
		t.Fatalf("nil lease close error = %v", err)
	}
}

func TestNativeResumeLeaseRejectsInvalidMutationRecordsAndCanceledWork(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	intent := nativeTestIntent(t, root, 0xd6, 0xd7)
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := repository.Acquire(context.Background(), intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	if err := lease.InstallReceipt(context.Background(), []byte("invalid")); !errors.Is(err, checkpointmodel.ErrInvalidReceipt) {
		t.Fatalf("invalid receipt error = %v", err)
	}
	if err := lease.ReplaceLifecycle(context.Background(), nil, nil); !errors.Is(err, checkpointmodel.ErrInvalidLifecycleState) {
		t.Fatalf("invalid lifecycle replacement error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lease.Snapshot(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot error = %v", err)
	}
	if _, err := lease.ObserveRecovery(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled recovery error = %v", err)
	}
	if _, err := lease.CleanupOwned(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup error = %v", err)
	}
	if err := lease.InstallReceipt(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled receipt error = %v", err)
	}
	if err := lease.ReplaceLifecycle(canceled, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lifecycle error = %v", err)
	}
}

func TestNativeResumePlatformAndDirectoryContractsFailClosed(t *testing.T) {
	failure := errors.New("injected platform failure")
	root := newRuntimeTestRootSpec(t).path
	tests := []struct {
		name    string
		context context.Context
		factory PlatformFactory
		want    error
	}{
		{
			name: "factory failure",
			factory: func(string, bool) (outputcap.Platform, error) {
				return nil, failure
			},
			want: failure,
		},
		{
			name: "missing platform",
			factory: func(string, bool) (outputcap.Platform, error) {
				return nil, nil
			},
			want: outputcap.ErrRecoverableOutputUnsupported,
		},
		{
			name: "missing root",
			factory: wrappedResumeTestPlatformFactory(t, root, func(base outputcap.Platform) outputcap.Platform {
				return &coverageC6Platform{Platform: base, overrideRoot: true}
			}),
			want: outputcap.ErrRecoverableOutputUnsupported,
		},
		{
			name: "invalid create authority",
			factory: wrappedResumeTestPlatformFactory(t, root, func(base outputcap.Platform) outputcap.Platform {
				return &coverageC6Platform{
					Platform:     base,
					overrideRoot: true,
					root:         coverageC6CreateAuthorityDirectory{Directory: base.Root(), err: failure},
				}
			}),
			want: failure,
		},
		{
			name: "feature probe failure",
			factory: wrappedResumeTestPlatformFactory(t, root, func(base outputcap.Platform) outputcap.Platform {
				return &coverageC6Platform{Platform: base, probeErr: failure}
			}),
			want: failure,
		},
		{
			name: "volatile durability",
			factory: wrappedResumeTestPlatformFactory(t, root, func(base outputcap.Platform) outputcap.Platform {
				return &coverageC6Platform{
					Platform: base, overrideDurability: true, durability: transfer.DurabilityNone,
				}
			}),
			want: outputcap.ErrRecoverableOutputUnsupported,
		},
		{
			name: "missing root binding",
			factory: wrappedResumeTestPlatformFactory(t, root, func(base outputcap.Platform) outputcap.Platform {
				return &coverageC6Platform{Platform: base, overrideBinding: true}
			}),
			want: transfer.ErrInvalidOutputBinding,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := test.context
			if ctx == nil {
				ctx = context.Background()
			}
			repository := &NativeResumeRepository{rootPath: root, platformFactory: test.factory}
			platform, authority, err := repository.openPlatform(ctx)
			if platform != nil || !authority.IsZero() || !errors.Is(err, test.want) {
				t.Fatalf("open platform = (%v, %v, %v)", platform, authority, err)
			}
		})
	}

	platform, err := openOutputRuntimeTestPlatform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := validatedNativeResumeDirectoryComponents(canceled, platform, "child"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled directory validation error = %v", err)
	}
	if components, err := validatedNativeResumeDirectoryComponents(context.Background(), platform, ""); err != nil || components != nil {
		t.Fatalf("root components = (%v, %v)", components, err)
	}
	if _, err := validatedNativeResumeDirectoryComponents(context.Background(), platform, "child/../other"); !errors.Is(err, checkpointmodel.ErrInvalidAdmittedDirectory) {
		t.Fatalf("noncanonical directory error = %v", err)
	}
	components, err := validatedNativeResumeDirectoryComponents(context.Background(), platform, "parent/child")
	if err != nil || len(components) != 2 || components[0] != "parent" || components[1] != "child" {
		t.Fatalf("directory components = (%v, %v)", components, err)
	}

	var pin *nativeResumeDirectoryPin
	if stable, err := pin.Revalidate(); stable || !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil pin revalidation = (%t, %v)", stable, err)
	}
	if stable, err := pin.RevalidateLineage(); stable || !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil pin lineage = (%t, %v)", stable, err)
	}
	if err := pin.Close(); err != nil {
		t.Fatalf("nil pin close error = %v", err)
	}
}

func TestNativeResumeObservationTaxonomyIsClosed(t *testing.T) {
	original := []byte("operation")
	snapshot := uncertainNativeResumeSnapshot(original)
	original[0] = 'X'
	if string(snapshot.OperationRecord) != "operation" || !bytes.Equal(snapshot.LifecycleRecord, []byte{0}) {
		t.Fatalf("uncertain snapshot = %+v", snapshot)
	}
	evidence := unknownNativeResumeEvidence(checkpointmodel.ReceiveLifecycleState{})
	if evidence.TargetOwnership != NativeResumeEvidenceUnknown ||
		evidence.Checkpoints != NativeResumeEvidenceUnknown || evidence.Cleanup != NativeResumeCleanupUnknown {
		t.Fatalf("unknown evidence = %+v", evidence)
	}

	uncertain := []error{
		ErrNativeResumeOwnershipUnknown,
		fs.ErrNotExist,
		outputcap.ErrUnsafeNamespace,
		outputcap.ErrNamespaceCollision,
		outputcap.ErrRecoverableOutputUnsupported,
		directoryauthority.ErrRetainedAuthorityChanged,
		checkpointmodel.ErrInvalidRecord,
		checkpointmodel.ErrInvalidLifecycleState,
	}
	for _, err := range uncertain {
		if !nativeResumeUncertain(err) {
			t.Fatalf("uncertainty error was treated as authoritative: %v", err)
		}
	}
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, ErrNativeResumeBusy, errors.New("ordinary")} {
		if nativeResumeUncertain(err) {
			t.Fatalf("ordinary control error became uncertainty: %v", err)
		}
	}
	if discard, err := nativeResumeUnprovenDiscard(nil); err != nil || discard.State != NativeResumeCleanupUnknown {
		t.Fatalf("unproven discard = (%+v, %v)", discard, err)
	}
	failure := errors.New("ordinary failure")
	if discard, err := nativeResumeUnprovenDiscard(failure); discard.State != 0 || !errors.Is(err, failure) {
		t.Fatalf("failed discard = (%+v, %v)", discard, err)
	}
	if _, err := nativeResumeEvidenceDigest(
		"", checkpointmodel.ReceiveOperation{}, checkpointmodel.ReceiveLifecycleState{}, nil, nil,
	); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("invalid evidence digest error = %v", err)
	}
	if _, err := validateNativeResumePublication(
		context.Background(), nil, nil, checkpointmodel.Record{},
	); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("invalid publication error = %v", err)
	}
	if err := errors.Join(
		closeNativeResumeFile(nil),
		closeNativeResumeGuard(nil),
		closeNativeResumeOwnedFile(nil),
	); err != nil {
		t.Fatalf("nil capability close error = %v", err)
	}
}

func TestNativeResumeSnapshotReadersRejectStructuralAmbiguity(t *testing.T) {
	failure := errors.New("directory failure")
	tests := []struct {
		name      string
		directory outputcap.Directory
		entryName string
		want      error
	}{
		{name: "nil parent", entryName: "child", want: transfer.ErrInvalidOutputBinding},
		{
			name:      "classification failure",
			directory: &resumeSnapshotDirectory{classifyErr: failure},
			entryName: "child",
			want:      failure,
		},
		{
			name:      "missing entry",
			directory: &resumeSnapshotDirectory{kind: outputcap.EntryAbsent, exact: true},
			entryName: "child",
			want:      fs.ErrNotExist,
		},
		{
			name:      "ambiguous entry",
			directory: &resumeSnapshotDirectory{kind: outputcap.EntryDirectory},
			entryName: "child",
			want:      outputcap.ErrUnsafeNamespace,
		},
		{
			name:      "wrong entry kind",
			directory: &resumeSnapshotDirectory{kind: outputcap.EntryRegularFile, exact: true},
			entryName: "child",
			want:      outputcap.ErrUnsafeNamespace,
		},
		{
			name:      "missing opened capability",
			directory: &resumeSnapshotDirectory{kind: outputcap.EntryDirectory, exact: true},
			entryName: "child",
			want:      outputcap.ErrUnsafeNamespace,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opened, err := openNativeResumeDirectory(test.directory, test.entryName, true)
			if opened != nil || !errors.Is(err, test.want) {
				t.Fatalf("open directory = (%v, %v)", opened, err)
			}
		})
	}

	if names, err := nativeResumeOperationNames(nil); names != nil ||
		!errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil operation inventory = (%v, %v)", names, err)
	}
	nameFailure := &resumeSnapshotDirectory{namesErr: failure}
	if names, err := nativeResumeOperationNames(nameFailure); names != nil || !errors.Is(err, failure) {
		t.Fatalf("failed operation inventory = (%v, %v)", names, err)
	}
	limit := &resumeSnapshotDirectory{names: make([]string, checkpointstore.EntryLimit)}
	if names, err := nativeResumeOperationNames(limit); names != nil || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("oversized operation inventory = (%v, %v)", names, err)
	}
	invalidHex := "gggggggggggggggggggggggggggggggg"
	if _, err := parseNativeResumeOperationName(invalidHex); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("invalid operation name error = %v", err)
	}

	repository := &NativeResumeRepository{}
	if snapshots, err := repository.listUncertainNamespace(
		context.Background(), nil, failure,
	); snapshots != nil || !errors.Is(err, failure) {
		t.Fatalf("ordinary namespace failure = (%v, %v)", snapshots, err)
	}
	if snapshots, err := repository.listUncertainNamespace(
		context.Background(), nil, ErrNativeResumeOwnershipUnknown,
	); snapshots != nil || !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("unreadable uncertain namespace = (%v, %v)", snapshots, err)
	}

	root := newRuntimeTestRootSpec(t).path
	_ = nativeTestIntent(t, root, 0xde, 0xdf)
	platform, err := openOutputRuntimeTestPlatform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if snapshots, err := repository.listUncertainNamespace(
		canceled, platform.Root(), ErrNativeResumeOwnershipUnknown,
	); snapshots != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled uncertain inventory = (%v, %v)", snapshots, err)
	}
	operations, err := openNativeResumeOperations(platform.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer operations.Close()
	unknown := incrementalTestIdentity16[receivecontract.OperationID](0xe0)
	if lifecycle, err := readNativeResumeLifecycle(operations, unknown); lifecycle != nil ||
		!errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing lifecycle = (%v, %v)", lifecycle, err)
	}
}

func wrappedResumeTestPlatformFactory(
	t *testing.T,
	root string,
	wrap func(outputcap.Platform) outputcap.Platform,
) PlatformFactory {
	t.Helper()
	return func(path string, create bool) (outputcap.Platform, error) {
		if filepath.Clean(path) != filepath.Clean(root) {
			return nil, transfer.ErrInvalidOutputBinding
		}
		base, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return wrap(base), nil
	}
}

type resumeSnapshotDirectory struct {
	outputcap.Directory

	kind        outputcap.EntryKind
	exact       bool
	classifyErr error
	opened      outputcap.Directory
	openErr     error
	names       []string
	namesErr    error
}

func (directory *resumeSnapshotDirectory) ClassifyExactEntry(
	string,
) (outputcap.EntryKind, bool, error) {
	return directory.kind, directory.exact, directory.classifyErr
}

func (directory *resumeSnapshotDirectory) OpenDirectory(string, bool) (outputcap.Directory, error) {
	return directory.opened, directory.openErr
}

func (directory *resumeSnapshotDirectory) Names(int) ([]string, error) {
	return directory.names, directory.namesErr
}
