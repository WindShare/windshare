//go:build windows || linux

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func TestCoverageC6FilesystemOutputFacadeProjectsLeaseAndPersistedRootDisposition(t *testing.T) {
	root := filepath.Join(t.TempDir(), "authority-created-output")
	intent := coverageC6FilesystemIntent(t, root, transfer.OutputNativeTree, 0xa1)
	var traceMu sync.Mutex
	var traces []FilesystemOutputTrace
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath:   root,
		CreateRoot: true,
		Tracer: FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
			traceMu.Lock()
			traces = append(traces, event)
			traceMu.Unlock()
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := authority.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.OpenOutput(context.Background(), intent); err == nil {
		t.Fatal("second facade session acquired the live intent lease")
	} else {
		result := transferfault.NormalizeBoundary(context.Background(), err)
		fault, ok := result.Fault()
		code, checkpoint := fault.CheckpointCode()
		if !ok || !checkpoint || code != transferfault.CheckpointBusy ||
			fault.Scope() != transferfault.ScopeOutputPause {
			t.Fatalf("facade contention fault = %v", err)
		}
	}
	if _, err := first.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	reopened, err := authority.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatalf("facade lease was not released: %v", err)
	}
	if _, err := reopened.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}

	traceMu.Lock()
	defer traceMu.Unlock()
	var sessionOpens, acquired, contended, released int
	for _, event := range traces {
		switch {
		case event.Operation == TraceSessionOpened:
			sessionOpens++
			if event.RootOpenDisposition != FilesystemOutputAuthorityCreatedRoot {
				t.Fatalf("session-open disposition = %q", event.RootOpenDisposition)
			}
		case event.Operation == TraceNativeLock && event.NativeLockMilestone == FilesystemOutputNativeLockAcquired:
			acquired++
		case event.Operation == TraceNativeLock && event.NativeLockMilestone == FilesystemOutputNativeLockContended:
			contended++
			if !event.Failed || event.FaultDomain != uint8(transferfault.DomainCheckpoint) ||
				event.NormalizedFaultScope != uint8(transferfault.ScopeOutputPause) ||
				event.NormalizedFaultCode != uint16(transferfault.CheckpointBusy) {
				t.Fatalf("contended trace projection = %+v", event)
			}
		case event.Operation == TraceNativeLock && event.NativeLockMilestone == FilesystemOutputNativeLockReleased:
			released++
		}
	}
	if sessionOpens != 2 || acquired != 2 || contended != 1 || released != 2 {
		t.Fatalf("facade trace counts opens/acquired/contended/released = %d/%d/%d/%d",
			sessionOpens, acquired, contended, released)
	}
}

func TestCoverageC6FilesystemOutputFacadeRejectsStreamWithoutCreatingAuthority(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-remain-absent")
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath:   root,
		CreateRoot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	intent := coverageC6FilesystemIntent(t, root, transfer.OutputSingleFileStream, 0xa2)
	if session, err := authority.OpenOutput(context.Background(), intent); session != nil ||
		!errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("stream facade open = (%T, %v)", session, err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stream open created native root authority: %v", err)
	}
}

func TestCoverageC6FilesystemResumeFacadeListsAndDiscardsPausedState(t *testing.T) {
	rootFixture := testoutputroot.New(t)
	if err := os.Mkdir(rootFixture.RootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	intent := coverageC6FilesystemIntent(t, rootFixture.RootPath, transfer.OutputNativeTree, 0xa3)
	output, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: rootFixture.RootPath})
	if err != nil {
		t.Fatal(err)
	}
	session, err := output.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	rootAdmission, err := session.AdmitDirectory(context.Background(), transfer.OutputDirectory{
		DirectoryID: intent.SyntheticRoot(),
		Generation:  coverageC6Identity[catalog.DirectoryGeneration](0xa4),
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := coverageC6Descriptor(t, intent, 8)
	locator, err := transfer.NewPathOutputLocator("paused.bin")
	if err != nil {
		t.Fatal(err)
	}
	target, err := transfer.NewOutputFileTarget(
		transfer.NativeFilesystemOutputBackendID, session.SessionID(), descriptor, locator,
	)
	if err != nil {
		t.Fatal(err)
	}
	start, err := session.BeginFile(context.Background(), transfer.OutputFile{
		Descriptor: descriptor, ExpectedSize: descriptor.ExactSize(),
		ParentAdmission: rootAdmission, Path: "paused.bin", Target: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatalf("paused file settled before transfer: %+v", start)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if settlement, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal(err)
	} else {
		checkpoint, ok := settlement.VerifiedCheckpoint()
		if settlement.Kind() != transfer.FilePaused || !ok || len(checkpoint.Ranges().Ranges()) != 1 {
			t.Fatalf("pause settlement = %+v", settlement)
		}
	}

	resume, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: rootFixture.RootPath})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := resume.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	summaries := inventory.Summaries()
	if len(summaries) != 1 || summaries[0].Status() != ResumeStateAvailable ||
		summaries[0].CheckpointRecordCount() != 0 ||
		summaries[0].Backend() != transfer.NativeFilesystemOutputBackendID {
		_ = inventory.Close()
		t.Fatalf("paused facade summaries = %+v", summaries)
	}
	if _, err := session.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		_ = inventory.Close()
		t.Fatal(err)
	}
	result, err := resume.Discard(context.Background(), summaries[0].Reference())
	if err != nil {
		_ = inventory.Close()
		t.Fatal(err)
	}
	if result.Status() != ResumeStateDiscarded || result.RemovedArtifacts() == 0 {
		_ = inventory.Close()
		t.Fatalf("paused facade discard = %+v", result)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(rootFixture.RootPath, "paused.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discard published or retained paused final: %v", err)
	}

	inventory, err = resume.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	summaries = inventory.Summaries()
	if len(summaries) != 1 || summaries[0].CheckpointRecordCount() != 0 {
		_ = inventory.Close()
		t.Fatalf("settled facade summaries = %+v", summaries)
	}
	result, err = resume.Discard(context.Background(), summaries[0].Reference())
	if err != nil {
		_ = inventory.Close()
		t.Fatal(err)
	}
	if result.Status() != ResumeStateAlreadyAbsent || result.RemovedArtifacts() != 0 {
		_ = inventory.Close()
		t.Fatalf("idempotent facade discard = %+v", result)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCoverageC6FilesystemResumeFacadeRejectsInvalidAndCanceledAuthority(t *testing.T) {
	var nilAuthority *FilesystemResumeStateAuthority
	if inventory, err := nilAuthority.ListResumeState(context.Background()); inventory != nil ||
		!errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil resume list = (%v, %v)", inventory, err)
	}
	if result, err := nilAuthority.Discard(context.Background(), ResumeStateRef{}); result.Status().Valid() ||
		!errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil resume discard = (%+v, %v)", result, err)
	}

	root := filepath.Join(t.TempDir(), "missing-root")
	authority, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if inventory, err := authority.ListResumeState(ctx); inventory != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resume list = (%v, %v)", inventory, err)
	}
	if inventory, err := authority.ListResumeState(context.Background()); inventory != nil || err == nil {
		t.Fatalf("missing-root resume list = (%v, %v)", inventory, err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only resume list created missing root: %v", err)
	}
}

func coverageC6FilesystemIntent(
	t *testing.T,
	root string,
	format transfer.OutputMode,
	seed byte,
) transfer.TransferIntent {
	t.Helper()
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewFilesystemTransferIntent(
		coverageC6Identity[catalog.ShareInstance](seed),
		coverageC6Identity[catalog.DirectoryID](seed+1),
		rules, root, transfer.NativeFilesystemOutputBackendID, format,
	)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func coverageC6Descriptor(
	t *testing.T,
	intent transfer.TransferIntent,
	exactSize uint64,
) content.FileRevisionDescriptor {
	t.Helper()
	geometry, err := content.NewFileGeometry(exactSize, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(),
		coverageC6Identity[catalog.FileID](0xa5),
		coverageC6Identity[content.FileRevision](0xa6),
		geometry,
		catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func coverageC6Identity[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var identity T
	for index := range identity {
		identity[index] = seed + byte(index)
	}
	return identity
}
