package outputruntime

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestNativeResumeRepositoryRejectsInvalidAndCancelledBoundaries(t *testing.T) {
	if _, err := NewNativeResumeRepository("relative", openOutputRuntimeTestPlatform); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("relative root = %v", err)
	}
	root := newRuntimeTestRootSpec(t).path
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repository.Page(cancelled, resumeauthority.PageCursor{}, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled page = %v", err)
	}
	if _, err := repository.Acquire(cancelled, incrementalTestIdentity16[receivecontract.OperationID](0x21)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire = %v", err)
	}
	if _, err := repository.Acquire(context.Background(), receivecontract.OperationID{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero acquire = %v", err)
	}
}

func TestNativeResumeInventoryDoesNotCreateRegistryForLiveOnlyDestination(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	factory := func(path string, create bool) (outputcap.Platform, error) {
		base, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return &liveOnlyRuntimePlatform{Platform: base}, nil
	}
	authority, err := New(Config{RootPath: root, PlatformFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	mode, err := authority.BindDestination(context.Background())
	if err != nil || !mode.LiveOnly() {
		t.Fatalf("live bind = %+v, %v", mode, err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewNativeResumeRepository(root, factory)
	page, err := repository.Page(context.Background(), resumeauthority.PageCursor{}, 1)
	if err != nil || len(page.Headers()) != 0 {
		t.Fatalf("live page = %d, %v", len(page.Headers()), err)
	}
	if _, err := os.Stat(filepath.Join(
		root, checkpointstore.ControlDirectory,
		checkpointstore.OrdinaryRegistryDirectoryV1,
	)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("resume inventory created ordinary registry: %v", err)
	}
}

func TestNativeResumeCleanupFailureClassificationIsOwnershipSafe(t *testing.T) {
	state, err := nativeResumeCleanupFailure(outputcap.ErrUnsafeNamespace)
	if err != nil || state != resumeauthority.CleanupPending {
		t.Fatalf("unsafe cleanup = %d, %v", state, err)
	}
	ioErr := errors.New("disk unavailable")
	state, err = nativeResumeCleanupFailure(ioErr)
	if !errors.Is(err, ioErr) || state != resumeauthority.CleanupPending {
		t.Fatalf("I/O cleanup = %d, %v", state, err)
	}
	if !nativeResumeUncertain(checkpointmodel.ErrRecordBinding) ||
		nativeResumeUncertain(context.Canceled) {
		t.Fatal("uncertainty classifier crossed cancellation/record boundary")
	}
	if !errors.Is(nativeResumeError(outputcap.ErrNamespaceLockBusy), resumeauthority.ErrBusy) {
		t.Fatal("native lock busy was not normalized")
	}
}
