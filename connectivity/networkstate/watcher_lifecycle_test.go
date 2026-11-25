package networkstate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func (s *countingSource) setGate(entered, block chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entered = entered
	s.block = block
}

func TestWatcherUnsubscribeAndCloseDoNotWaitForUnrelatedSnapshot(t *testing.T) {
	t.Run("unsubscribe_exits_promptly_and_resets_gap_when_flight_settles", func(t *testing.T) {
		now := time.Unix(20000, 0)
		var clockMu sync.Mutex
		clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
		advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

		// Interval*activeFreshnessMultiplier (6s) < ResumeGap (10s), so forcing a
		// cache miss on Snapshot while active does not synthesize an OS sleep.
		source := &countingSource{}
		source.setIP("10.0.0.2")
		watcher := NewWatcher(WatcherConfig{
			Source:   source,
			Debounce: time.Nanosecond,
			Interval: 3 * time.Second,
			Now:      clock,
		})
		defer watcher.Close()

		// Establish baseline before subscribing so run() deterministically skips initialPoll.
		if snap, err := watcher.Snapshot(context.Background()); err != nil || snap.GenerationID() != 1 {
			t.Fatalf("baseline Snapshot = (%d, %v)", snap.GenerationID(), err)
		}
		_, unsub := watcher.Subscribe()
		if snap, changed, err := watcher.Step(context.Background()); err != nil || changed || snap.GenerationID() != 1 {
			t.Fatalf("active baseline Step = (%d, %t, %v)", snap.GenerationID(), changed, err)
		}

		// Force a cache miss while staying within ResumeGap so an unrelated Snapshot
		// owns the in-flight slot without triggering an active sleep.
		advance(6 * time.Second)
		entered := make(chan struct{}, 1)
		block := make(chan struct{})
		source.setGate(entered, block)

		snapDone := make(chan error, 1)
		go func() {
			_, err := watcher.Snapshot(context.Background())
			snapDone <- err
		}()
		<-entered

		unsubDone := make(chan struct{})
		go func() {
			unsub()
			close(unsubDone)
		}()

		select {
		case <-unsubDone:
		case <-time.After(time.Second):
			close(block)
			<-snapDone
			t.Fatal("unsubscribe blocked on unrelated in-flight Snapshot")
		}

		source.setGate(nil, nil)
		close(block)
		if err := <-snapDone; err != nil {
			t.Fatalf("in-flight Snapshot err = %v", err)
		}

		// After the in-flight sample settles while idle, a long idle pause must not
		// trigger a false OS sleep on the next active subscription.
		advance(ResumeGap + time.Minute)
		_, unsub2 := watcher.Subscribe()
		defer unsub2()
		if snap, changed, err := watcher.Step(context.Background()); err != nil || changed || snap.GenerationID() != 1 {
			t.Fatalf("post-idle Step triggered false resume: (%d, %t, %v)", snap.GenerationID(), changed, err)
		}
		advance(time.Second)
		if snap, changed, err := watcher.Step(context.Background()); err != nil || changed || snap.GenerationID() != 1 {
			t.Fatalf("post-idle debounce Step triggered false resume: (%d, %t, %v)", snap.GenerationID(), changed, err)
		}
	})

	t.Run("cold_start_unsubscribe_exits_promptly_and_preserves_active_sleep_detection", func(t *testing.T) {
		now := time.Unix(20500, 0)
		var clockMu sync.Mutex
		clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
		advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

		entered := make(chan struct{}, 1)
		block := make(chan struct{})
		source := &countingSource{entered: entered, block: block}
		source.setIP("10.0.0.2")
		watcher := NewWatcher(WatcherConfig{
			Source:   source,
			Debounce: time.Nanosecond,
			Interval: time.Hour,
			Now:      clock,
		})
		defer watcher.Close()

		snapDone := make(chan error, 1)
		go func() {
			_, err := watcher.Snapshot(context.Background())
			snapDone <- err
		}()
		<-entered

		_, unsub := watcher.Subscribe()
		unsubDone := make(chan struct{})
		go func() {
			unsub()
			close(unsubDone)
		}()

		select {
		case <-unsubDone:
		case <-time.After(time.Second):
			close(block)
			<-snapDone
			t.Fatal("cold-start unsubscribe blocked on unrelated in-flight Snapshot")
		}

		source.setGate(nil, nil)
		close(block)
		if err := <-snapDone; err != nil {
			t.Fatalf("cold-start Snapshot err = %v", err)
		}

		advance(ResumeGap + time.Minute)
		_, unsub2 := watcher.Subscribe()
		defer unsub2()
		if snap, changed, err := watcher.Step(context.Background()); err != nil || changed || snap.GenerationID() != 1 {
			t.Fatalf("post-idle Step triggered false resume: (%d, %t, %v)", snap.GenerationID(), changed, err)
		}

		// Active sleep detection remains intact once polling resumes.
		advance(ResumeGap + time.Second)
		if _, changed, err := watcher.Step(context.Background()); err != nil || changed {
			t.Fatalf("resume debounce Step = (%t, %v)", changed, err)
		}
		advance(time.Second)
		if snap, changed, err := watcher.Step(context.Background()); err != nil || !changed || snap.GenerationID() != 2 {
			t.Fatalf("active sleep was not detected: (%d, %t, %v)", snap.GenerationID(), changed, err)
		}
	})

	t.Run("close_exits_promptly_while_unrelated_snapshot_blocks", func(t *testing.T) {
		now := time.Unix(21000, 0)
		var clockMu sync.Mutex
		clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
		advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

		source := &countingSource{}
		source.setIP("10.0.0.2")
		watcher := NewWatcher(WatcherConfig{
			Source:   source,
			Debounce: time.Nanosecond,
			Interval: 3 * time.Second,
			Now:      clock,
		})

		if snap, err := watcher.Snapshot(context.Background()); err != nil || snap.GenerationID() != 1 {
			watcher.Close()
			t.Fatalf("baseline Snapshot = (%d, %v)", snap.GenerationID(), err)
		}
		ch, _ := watcher.Subscribe()

		advance(6 * time.Second)
		entered := make(chan struct{}, 1)
		block := make(chan struct{})
		source.setGate(entered, block)

		snapDone := make(chan error, 1)
		go func() {
			_, err := watcher.Snapshot(context.Background())
			snapDone <- err
		}()
		<-entered

		closeDone := make(chan struct{})
		go func() {
			watcher.Close()
			close(closeDone)
		}()

		select {
		case <-closeDone:
		case <-time.After(time.Second):
			close(block)
			<-snapDone
			t.Fatal("Close blocked on unrelated in-flight Snapshot")
		}
		if _, ok := <-ch; ok {
			close(block)
			<-snapDone
			t.Fatal("Close did not close subscriber channel")
		}

		source.setGate(nil, nil)
		close(block)
		if err := <-snapDone; err != nil {
			t.Fatalf("in-flight Snapshot after Close err = %v", err)
		}
	})

	t.Run("resubscribe_before_old_flight_finishes_still_clears_poll_gap", func(t *testing.T) {
		now := time.Unix(22000, 0)
		var clockMu sync.Mutex
		clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
		advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

		source := &countingSource{}
		source.setIP("10.0.0.2")
		watcher := NewWatcher(WatcherConfig{
			Source:   source,
			Debounce: time.Nanosecond,
			Interval: time.Hour,
			Now:      clock,
		})
		defer watcher.Close()

		if snap, err := watcher.Snapshot(context.Background()); err != nil || snap.GenerationID() != 1 {
			t.Fatalf("baseline Snapshot = (%d, %v)", snap.GenerationID(), err)
		}
		_, unsub1 := watcher.Subscribe()
		if snap, changed, err := watcher.Step(context.Background()); err != nil || changed || snap.GenerationID() != 1 {
			t.Fatalf("active baseline Step = (%d, %t, %v)", snap.GenerationID(), changed, err)
		}

		// Start an unrelated Step at t+1s that blocks across unsubscribe and resubscribe.
		advance(time.Second)
		entered := make(chan struct{}, 1)
		block := make(chan struct{})
		source.setGate(entered, block)

		stepDone := make(chan error, 1)
		go func() {
			_, _, err := watcher.Step(context.Background())
			stepDone <- err
		}()
		<-entered

		unsub1()

		// Resubscribe after an idle gap > ResumeGap while the pre-idle flight is still blocked.
		advance(ResumeGap + time.Minute)
		_, unsub2 := watcher.Subscribe()
		defer unsub2()

		source.setGate(nil, nil)
		close(block)
		if err := <-stepDone; err != nil {
			t.Fatalf("pre-idle Step err = %v", err)
		}

		// The pre-idle flight captured the old timestamp; releasePoll must still clear
		// lastPoll so the new active subscription does not synthesize an OS sleep.
		if snap, changed, err := watcher.Step(context.Background()); err != nil || changed || snap.GenerationID() != 1 {
			t.Fatalf("resubscribe Step triggered false resume: (%d, %t, %v)", snap.GenerationID(), changed, err)
		}
		advance(time.Second)
		if snap, changed, err := watcher.Step(context.Background()); err != nil || changed || snap.GenerationID() != 1 {
			t.Fatalf("post-debounce resubscribe Step triggered false resume: (%d, %t, %v)", snap.GenerationID(), changed, err)
		}
	})

	t.Run("canceled_flight_across_unsubscribe_still_clears_prior_poll_gap", func(t *testing.T) {
		now := time.Unix(23000, 0)
		var clockMu sync.Mutex
		clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
		advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

		source := &countingSource{}
		source.setIP("10.0.0.2")
		watcher := NewWatcher(WatcherConfig{
			Source:   source,
			Debounce: time.Nanosecond,
			Interval: time.Hour,
			Now:      clock,
		})
		defer watcher.Close()

		if snap, err := watcher.Snapshot(context.Background()); err != nil || snap.GenerationID() != 1 {
			t.Fatalf("baseline Snapshot = (%d, %v)", snap.GenerationID(), err)
		}
		_, unsub1 := watcher.Subscribe()
		if snap, changed, err := watcher.Step(context.Background()); err != nil || changed || snap.GenerationID() != 1 {
			t.Fatalf("active baseline Step = (%d, %t, %v)", snap.GenerationID(), changed, err)
		}

		advance(time.Second)
		entered := make(chan struct{}, 1)
		block := make(chan struct{})
		source.setGate(entered, block)

		flightCtx, cancelFlight := context.WithCancel(context.Background())
		flightDone := make(chan error, 1)
		go func() {
			_, _, err := watcher.Step(flightCtx)
			flightDone <- err
		}()
		<-entered

		unsub1()
		cancelFlight()
		if err := <-flightDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled flight err = %v, want Canceled", err)
		}
		source.setGate(nil, nil)

		advance(ResumeGap + time.Minute)
		_, unsub2 := watcher.Subscribe()
		defer unsub2()
		if snap, changed, err := watcher.Step(context.Background()); err != nil || changed || snap.GenerationID() != 1 {
			t.Fatalf("Step after canceled pre-idle flight triggered false resume: (%d, %t, %v)", snap.GenerationID(), changed, err)
		}
		advance(time.Second)
		if snap, changed, err := watcher.Step(context.Background()); err != nil || changed || snap.GenerationID() != 1 {
			t.Fatalf("post-debounce Step triggered false resume: (%d, %t, %v)", snap.GenerationID(), changed, err)
		}
	})

	t.Run("idle_snapshot_does_not_arm_resume_gap", func(t *testing.T) {
		now := time.Unix(24000, 0)
		var clockMu sync.Mutex
		clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
		advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

		source := &countingSource{}
		source.setIP("10.0.0.2")
		watcher := NewWatcher(WatcherConfig{
			Source:   source,
			Debounce: time.Nanosecond,
			Interval: time.Second,
			Now:      clock,
		})
		defer watcher.Close()

		if snap, err := watcher.Snapshot(context.Background()); err != nil || snap.GenerationID() != 1 {
			t.Fatalf("idle Snapshot = (%d, %v)", snap.GenerationID(), err)
		}

		advance(ResumeGap + time.Minute)
		if snap, err := watcher.Snapshot(context.Background()); err != nil || snap.GenerationID() != 1 {
			t.Fatalf("second idle Snapshot = (%d, %v)", snap.GenerationID(), err)
		}
		advance(time.Second)
		if snap, err := watcher.Snapshot(context.Background()); err != nil || snap.GenerationID() != 1 {
			t.Fatalf("post-debounce idle Snapshot triggered false resume: (%d, %v)", snap.GenerationID(), err)
		}
	})
}
