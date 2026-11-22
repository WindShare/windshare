package outputruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestNativeResumeRepositoryRejectsInvalidAndCancelledBoundaries(t *testing.T) {
	if _, err := NewNativeResumeRepository("relative", openOutputRuntimeTestPlatform, nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("relative root = %v", err)
	}
	root := newRuntimeTestRootSpec(t).path
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform, nil)
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
