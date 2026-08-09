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
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestCoverageC6FilesystemOutputFacadeProjectsLeaseAndPersistedRootDisposition(t *testing.T) {
	root := filepath.Join(t.TempDir(), "authority-created-output")
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
	intent := coverageC6FilesystemIntent(t, authority, 0xa1)

	first, err := authority.OpenDirectTree(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.OpenDirectTree(context.Background(), intent); err == nil {
		t.Fatal("second facade session acquired the live operation lease")
	} else {
		result := transferfault.NormalizeBoundary(context.Background(), err)
		fault, ok := result.Fault()
		code, checkpoint := fault.CheckpointCode()
		if !ok || !checkpoint || code != transferfault.CheckpointBusy ||
			fault.Scope() != transferfault.ScopeOutputPause {
			t.Fatalf("facade contention fault = %v", err)
		}
	}
	if _, err := first.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	reopened, err := authority.OpenDirectTree(context.Background(), intent)
	if err != nil {
		t.Fatalf("facade lease was not released: %v", err)
	}
	if _, err := reopened.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}

	traceMu.Lock()
	defer traceMu.Unlock()
	var sessionOpens, acquired, contended, released int
	for _, event := range traces {
		if event.ReceiveIntentDigest != intent.Digest() && !event.ReceiveIntentDigest.IsZero() {
			t.Fatalf("trace crossed receive-intent authority: %+v", event)
		}
		switch {
		case event.Operation == TraceSessionOpened:
			sessionOpens++
			if event.RootOpenDisposition != FilesystemOutputAuthorityCreatedRoot ||
				event.ReceiveOperationID != intent.OperationID() {
				t.Fatalf("session-open projection = %+v", event)
			}
		case event.Operation == TraceNativeLock &&
			event.NativeLockMilestone == FilesystemOutputNativeLockAcquired:
			acquired++
		case event.Operation == TraceNativeLock &&
			event.NativeLockMilestone == FilesystemOutputNativeLockContended:
			contended++
			if !event.Failed || event.FaultDomain != uint8(transferfault.DomainCheckpoint) ||
				event.NormalizedFaultScope != uint8(transferfault.ScopeOutputPause) ||
				event.NormalizedFaultCode != uint16(transferfault.CheckpointBusy) {
				t.Fatalf("contended trace projection = %+v", event)
			}
		case event.Operation == TraceNativeLock &&
			event.NativeLockMilestone == FilesystemOutputNativeLockReleased:
			released++
		}
	}
	if sessionOpens != 2 || acquired != 2 || contended != 1 || released != 2 {
		t.Fatalf(
			"facade trace counts opens/acquired/contended/released = %d/%d/%d/%d",
			sessionOpens, acquired, contended, released,
		)
	}
}

func TestCoverageC6FilesystemOutputFacadeRejectsNonTreeArtifactWithoutCreatingAuthority(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-remain-absent")
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath: root, CreateRoot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(
		coverageC6Identity[catalog.ShareInstance](0xa2),
		coverageC6Identity[catalog.DirectoryID](0xa3),
		rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewOriginalFileArtifact(
		coverageC6Identity[catalog.FileID](0xa4), "source.bin", "source.bin",
	)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := authority.ReserveDirectTree(context.Background(), selection, artifact)
	if reservation.Kind() != 0 || !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("non-tree facade reservation = (%d, %v)", reservation.Kind(), err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected artifact created native root authority: %v", err)
	}
}

func coverageC6FilesystemIntent(
	t *testing.T,
	authority *FilesystemOutputAuthority,
	seed byte,
) transfer.ReceiveIntent {
	t.Helper()
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(
		coverageC6Identity[catalog.ShareInstance](seed),
		coverageC6Identity[catalog.DirectoryID](seed+1),
		rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := authority.ReserveDirectTree(
		context.Background(), selection, receivecontract.NewCatalogRootDirectoryTree(),
	)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := reservation.ReceiveIntent()
	if !ok {
		t.Fatalf("native DirectTree reservation kind = %d", reservation.Kind())
	}
	return intent
}

func coverageC6Identity[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var identity T
	for index := range identity {
		identity[index] = seed + byte(index)
	}
	return identity
}
