package share

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/engine/internal/task"
)

type fakeSleepInhibitor struct {
	acquires atomic.Int32
	releases atomic.Int32
	err      error
	entered  chan struct{}
	unblock  chan struct{}
}

func (f *fakeSleepInhibitor) InhibitSleep(_ string) (func(), error) {
	f.acquires.Add(1)
	if f.entered != nil {
		close(f.entered)
	}
	if f.unblock != nil {
		<-f.unblock
	}
	if f.err != nil {
		return nil, f.err
	}
	return func() { f.releases.Add(1) }, nil
}

func TestSleepGuardRefcountsConcurrentTransfersAndReleasesAfterCooldown(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		inhibitor := &fakeSleepInhibitor{}
		guard := newSleepGuard(inhibitor, diagnostics.Sink{})
		defer guard.close()

		leaveFirst := guard.BeginTransfer()
		leaveSecond := guard.BeginTransfer()
		if got := inhibitor.acquires.Load(); got != 1 {
			t.Fatalf("acquires = %d, want 1", got)
		}
		if got := inhibitor.releases.Load(); got != 0 {
			t.Fatalf("releases = %d, want 0", got)
		}

		leaveFirst()
		// Repeated leave calls must be idempotent and keep the second transfer protected.
		leaveFirst()
		time.Sleep(shareSleepInhibitCooldown + time.Second)
		synctest.Wait()
		if got := inhibitor.releases.Load(); got != 0 {
			t.Fatalf("releases while transfer active = %d, want 0", got)
		}

		leaveSecond()
		time.Sleep(shareSleepInhibitCooldown - time.Second)
		synctest.Wait()
		if got := inhibitor.releases.Load(); got != 0 {
			t.Fatalf("releases before cooldown elapsed = %d, want 0", got)
		}

		time.Sleep(time.Second)
		synctest.Wait()
		if got := inhibitor.releases.Load(); got != 1 {
			t.Fatalf("releases after cooldown elapsed = %d, want 1", got)
		}

		// Subsequent transfers acquire cleanly again.
		leaveThird := guard.BeginTransfer()
		if got := inhibitor.acquires.Load(); got != 2 {
			t.Fatalf("acquires on re-entry = %d, want 2", got)
		}
		leaveThird()
		time.Sleep(shareSleepInhibitCooldown)
		synctest.Wait()
		if got := inhibitor.releases.Load(); got != 2 {
			t.Fatalf("releases on re-entry exit = %d, want 2", got)
		}
	})
}

func TestSleepGuardReentryDuringCooldownAvoidsAssertionFlapping(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		inhibitor := &fakeSleepInhibitor{}
		guard := newSleepGuard(inhibitor, diagnostics.Sink{})
		defer guard.close()

		leaveFirst := guard.BeginTransfer()
		leaveFirst()

		time.Sleep(shareSleepInhibitCooldown / 2)
		synctest.Wait()

		leaveSecond := guard.BeginTransfer()
		leaveSecond()

		// Advancing past the original deadline must not release because re-entry reset cooldown.
		time.Sleep(shareSleepInhibitCooldown - time.Second)
		synctest.Wait()
		if got := inhibitor.acquires.Load(); got != 1 {
			t.Fatalf("acquires across sequential transfers = %d, want 1", got)
		}
		if got := inhibitor.releases.Load(); got != 0 {
			t.Fatalf("releases before reset cooldown elapsed = %d, want 0", got)
		}

		time.Sleep(time.Second)
		synctest.Wait()
		if got := inhibitor.releases.Load(); got != 1 {
			t.Fatalf("releases after reset cooldown elapsed = %d, want 1", got)
		}
	})
}

func TestSleepGuardCloseCancelsCooldownAndIgnoresLateEntry(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		inhibitor := &fakeSleepInhibitor{}
		guard := newSleepGuard(inhibitor, diagnostics.Sink{})

		leave := guard.BeginTransfer()
		leave()
		guard.close()

		if got := inhibitor.releases.Load(); got != 1 {
			t.Fatalf("releases after close during cooldown = %d, want 1", got)
		}

		lateLeave := guard.BeginTransfer()
		lateLeave()
		if got := inhibitor.acquires.Load(); got != 1 {
			t.Fatalf("acquires after close = %d, want 1", got)
		}
	})
}

func TestSleepGuardCloseJoinsInFlightAcquireAndDiscardsLateAssertion(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		inhibitor := &fakeSleepInhibitor{
			entered: make(chan struct{}),
			unblock: make(chan struct{}),
		}
		guard := newSleepGuard(inhibitor, diagnostics.Sink{})

		enterDone := make(chan func(), 1)
		go func() {
			enterDone <- guard.BeginTransfer()
		}()
		<-inhibitor.entered

		closeDone := make(chan struct{})
		go func() {
			guard.close()
			close(closeDone)
		}()
		synctest.Wait()
		select {
		case <-closeDone:
			t.Fatal("close returned while InhibitSleep was still in flight")
		default:
		}

		close(inhibitor.unblock)
		<-closeDone
		leave := <-enterDone
		leave()

		if got := inhibitor.acquires.Load(); got != 1 {
			t.Fatalf("acquires = %d, want 1", got)
		}
		if got := inhibitor.releases.Load(); got != 1 {
			t.Fatalf("releases = %d, want 1", got)
		}
	})
}

func TestSleepGuardRecordsAcquireFailureWithoutBlocking(t *testing.T) {
	t.Parallel()

	stream, err := diagnostics.NewStream(8, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	inhibitor := &fakeSleepInhibitor{err: errors.New("os assertion unavailable")}
	guard := newSleepGuard(inhibitor, stream.Sink())

	leave := guard.BeginTransfer()
	leave()
	guard.close()
	stream.Close()

	var transitions []string
	for record := range stream.Records() {
		if record.Name() == "share_sleep_inhibit" {
			transitions = append(transitions, record.Payload().Get("transition").Text())
		}
	}
	if len(transitions) != 1 || transitions[0] != "acquire_failed" {
		t.Fatalf("diagnostic transitions = %v, want [acquire_failed]", transitions)
	}
}

func TestShareRunWiresSleepInhibitorToTransferActivityAndReleasesOnStop(t *testing.T) {
	t.Parallel()

	f := newShareFixture(t)
	inhibitor := &fakeSleepInhibitor{}
	f.dependencies.SleepInhibitor = inhibitor

	activityCh := make(chan contentflow.TransferActivity, 1)
	f.prepared.observeRuntime = func(config liveshare.RuntimeFactoryConfig) {
		activityCh <- config.TransferActivity
	}

	ctx, stop := context.WithCancelCause(context.Background())
	done := fixtureRun(f, ctx, Request{})
	if _, err := f.dependencies.Controller.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}

	activity := <-activityCh
	if activity == nil {
		t.Fatal("share runtime omitted transfer activity hook")
	}
	if got := inhibitor.acquires.Load(); got != 0 {
		t.Fatalf("idle share acquired sleep inhibition %d time(s), want 0", got)
	}

	leave := activity.BeginTransfer()
	if got := inhibitor.acquires.Load(); got != 1 {
		t.Fatalf("acquires during transfer = %d, want 1", got)
	}
	leave()
	if got := inhibitor.releases.Load(); got != 0 {
		t.Fatalf("releases immediately after transfer = %d, want 0 during cooldown", got)
	}

	// Stopping the share releases the cooldown assertion immediately instead of
	// leaking a timer or OS assertion past share shutdown.
	stop(task.ErrShareStopped)
	result := awaitResult(t, done)
	if result.Err != nil || result.CleanupError != nil {
		t.Fatalf("result=%+v", result)
	}
	if got := inhibitor.releases.Load(); got != 1 {
		t.Fatalf("releases after share stop = %d, want 1", got)
	}
}
