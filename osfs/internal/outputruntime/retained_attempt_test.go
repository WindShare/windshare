package outputruntime

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestStoppedShareRetainsCheckpointsForExactDiscard(t *testing.T) {
	ctx := t.Context()
	root := newRuntimeTestRootSpec(t).path
	original := openOrdinaryResumeSession(t, root, 0x83, 4)
	t.Cleanup(func() { _ = original.authority.Close() })
	operation := original.authority.operation
	if err := operation.RecordShareStopped(ctx); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("stop recorded during open output session: %v", err)
	}
	if err := original.transaction.WriteRange(ctx, 0, []byte("da")); err != nil {
		t.Fatal(err)
	}
	if _, err := original.transaction.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := original.session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordShareStopped(ctx); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordShareStopped(ctx); err != nil {
		t.Fatalf("repeated stop recording: %v", err)
	}
	if _, err := original.authority.OpenOperation(ctx, operation); !errors.Is(err, ErrShareStopped) {
		t.Fatalf("stopped operation started an output session: %v", err)
	}
	if err := original.authority.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(Config{RootPath: root, PlatformFactory: openOutputRuntimeTestPlatform})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.LookupActive(ctx, original.intent.SelectionSpec()); !errors.Is(err, ErrShareStopped) {
		t.Fatalf("automatic lookup reopened stopped output: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform, nil)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := resumeauthority.New(repository)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := inventory.List(ctx)
	summaries := listed.Summaries()
	if err != nil || len(summaries) != 1 || summaries[0].State() != resumeauthority.OperationShareStopped {
		t.Fatalf("stopped inventory = %+v, err=%v", summaries, err)
	}
	items := summaries[0].Items()
	if len(items) != 1 || items[0].State() != resumeauthority.ItemResumable {
		t.Fatalf("stopped output lost its retained checkpoint: %+v", items)
	}
	discarded, err := inventory.Discard(ctx, original.intent.OperationID())
	if err != nil || discarded.State() != resumeauthority.OperationDiscarded {
		t.Fatalf("discarded=%+v err=%v", discarded, err)
	}
	after, err := inventory.List(ctx)
	if err != nil || len(after.Summaries()) != 0 {
		t.Fatalf("stopped record survived discard: %+v err=%v", after.Summaries(), err)
	}
}
