package fileexecution

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/transfer"
)

type controlledCheckpointTimer struct {
	flush   func()
	stopped bool
}

func (timer *controlledCheckpointTimer) Stop() bool {
	prior := timer.stopped
	timer.stopped = true
	return !prior
}
func (timer *controlledCheckpointTimer) fire() {
	if !timer.stopped {
		timer.flush()
	}
}

func TestCheckpointBatchesUntilBytesDeadlineOrPause(t *testing.T) {
	for _, trigger := range []string{"bytes", "idle deadline", "pause"} {
		t.Run(trigger, func(t *testing.T) {
			fixture := newExecutionFixture(t, CheckpointByteLimit+4)
			repository := &memoryCheckpointRepository{}
			engine := newFixtureEngine(t, fixture, repository, newMemoryPlatform(), &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}, nil)
			var timer *controlledCheckpointTimer
			engine.afterFunc = func(delay time.Duration, flush func()) CheckpointTimer {
				if delay != CheckpointInterval {
					t.Fatalf("unbounded checkpoint deadline: %s", delay)
				}
				timer = &controlledCheckpointTimer{flush: flush}
				return timer
			}
			engine.now = func() time.Time { return time.Unix(100, 0) }
			ctx := context.Background()
			start, err := engine.BeginFile(ctx, fixture.file, fixture.destination)
			if err != nil {
				t.Fatal(err)
			}
			transaction, _, _ := start.Transaction()
			if err := transaction.WriteRange(ctx, 0, []byte("part")); err != nil {
				t.Fatal(err)
			}
			progress, err := transaction.Accept(ctx)
			if err != nil {
				t.Fatal(err)
			}
			persisted, ok := progress.Persisted()
			if !ok || !persisted.Ranges().IsEmpty() || progress.Accepted().Ranges().IsEmpty() {
				t.Fatal("acceptance claimed an unsaved restart range")
			}
			initialStores := len(repository.stores)
			switch trigger {
			case "bytes":
				if err := transaction.WriteRange(ctx, 4, make([]byte, CheckpointByteLimit-4)); err != nil {
					t.Fatal(err)
				}
				if _, err := transaction.Accept(ctx); err != nil {
					t.Fatal(err)
				}
			case "idle deadline":
				timer.fire()
			case "pause":
				if _, err := transaction.Pause(ctx, transfer.FilePauseShutdown); err != nil {
					t.Fatal(err)
				}
			}
			ranges := repository.record.VerifiedRanges()
			want := uint64(4)
			if trigger == "bytes" {
				want = CheckpointByteLimit
			}
			if len(ranges) != 1 || ranges[0].Offset != 0 || ranges[0].End != want || len(repository.stores) <= initialStores || !timer.stopped {
				t.Fatalf("trigger %s did not save the exact partial: %+v", trigger, ranges)
			}
			if trigger != "pause" {
				if _, err := transaction.Pause(ctx, transfer.FilePauseShutdown); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestFailedIdleFlushKeepsPriorCheckpointAndClosesOnPause(t *testing.T) {
	fixture := newExecutionFixture(t, 8)
	repository := &memoryCheckpointRepository{}
	engine := newFixtureEngine(t, fixture, repository, newMemoryPlatform(), &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}, nil)
	var timer *controlledCheckpointTimer
	engine.afterFunc = func(_ time.Duration, flush func()) CheckpointTimer {
		timer = &controlledCheckpointTimer{flush: flush}
		return timer
	}
	ctx := context.Background()
	start, err := engine.BeginFile(ctx, fixture.file, fixture.destination)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, _ := start.Transaction()
	if err := transaction.WriteRange(ctx, 0, []byte("part")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	prior := repository.record.CanonicalBytes()
	if err := transaction.WriteRange(ctx, 4, []byte("tail")); err != nil {
		t.Fatal(err)
	}
	owned := transaction.(*PartialFileTransaction).strategy.(*resumablePartialFileTransaction).file.(*memoryOwnedFile)
	syncFailure := errors.New("data sync failed")
	owned.syncErr = syncFailure
	timer.fire()
	if !bytes.Equal(prior, repository.record.CanonicalBytes()) {
		t.Fatal("unsynced bytes advanced the durable checkpoint")
	}
	if _, err := transaction.Accept(ctx); !errors.Is(err, syncFailure) {
		t.Fatalf("flush failure was hidden: %v", err)
	}
	if _, err := transaction.Pause(ctx, transfer.FilePauseShutdown); !errors.Is(err, syncFailure) || !owned.closed {
		t.Fatalf("pause did not close failed writer: %v", err)
	}
	if !bytes.Equal(prior, repository.record.CanonicalBytes()) {
		t.Fatal("failed pause revoked the saved ranges")
	}
}
