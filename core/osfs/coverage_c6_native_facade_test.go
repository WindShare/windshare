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
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestCoverageC6FilesystemOutputFacadeProjectsLeaseAndPersistedRootDisposition(t *testing.T) {
	root := filepath.Join(t.TempDir(), "authority-created-output")
	var traceMu sync.Mutex
	var traces []FilesystemOutputTrace
	tracer := FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		traceMu.Lock()
		traces = append(traces, event)
		traceMu.Unlock()
	})
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath: root, CreateRoot: true, Tracer: tracer,
	})
	if err != nil {
		t.Fatal(err)
	}
	intent := coverageC6FilesystemIntent(t, authority, 0xa1)

	first, err := authority.OpenDirectTree(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	contender, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath: root, CreateRoot: true, Tracer: tracer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contender.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	lookup, err := contender.LookupActive(context.Background(), intent.SelectionSpec())
	if err != nil || lookup.Kind() != FilesystemOutputLookupAlreadyRunning {
		t.Fatalf("staged facade contention = (%d, %v)", lookup.Kind(), err)
	}
	if err := contender.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	reopener, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath: root, CreateRoot: true, Tracer: tracer,
	})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := reopener.ReserveDirectTree(
		context.Background(), intent.SelectionSpec(), receivecontract.NewCatalogRootDirectoryTree(),
	)
	if err != nil || reservation.Kind() != NativeDirectTreeReopened {
		t.Fatalf("facade reservation was not reopened: (%d, %v)", reservation.Kind(), err)
	}
	reopenedIntent, ok := reservation.ReceiveIntent()
	if !ok || !reopenedIntent.EqualCanonical(intent) {
		t.Fatal("facade reopen changed its frozen intent")
	}
	reopened, err := reopener.OpenDirectTree(context.Background(), reopenedIntent)
	if err != nil {
		t.Fatalf("facade lease was not released: %v", err)
	}
	if _, err := reopened.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := reopener.Close(); err != nil {
		t.Fatal(err)
	}

	traceMu.Lock()
	defer traceMu.Unlock()
	var destinationAdmitted, pauseMilestones int
	for _, event := range traces {
		if event.ReceiveIntentDigest != intent.Digest() && !event.ReceiveIntentDigest.IsZero() {
			t.Fatalf("trace crossed receive-intent authority: %+v", event)
		}
		if event.Operation != TraceRuntimeDecision {
			continue
		}
		switch event.RuntimeOperation {
		case FilesystemOutputRuntimeAdmitDestination:
			destinationAdmitted++
			if event.RuntimeDecision != FilesystemOutputRuntimeAdmitted ||
				event.ReceiveOperationID != intent.OperationID() {
				t.Fatalf("destination admission trace = %+v", event)
			}
		case FilesystemOutputRuntimePauseTree:
			pauseMilestones++
			if event.ReceiveOperationID != intent.OperationID() || event.SessionID.IsZero() {
				t.Fatalf("pause trace = %+v", event)
			}
		}
	}
	if destinationAdmitted != 2 || pauseMilestones < 2 {
		t.Fatalf(
			"facade trace counts destination-admitted/pause = %d/%d",
			destinationAdmitted, pauseMilestones,
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
