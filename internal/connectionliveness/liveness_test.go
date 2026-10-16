package connectionliveness

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestSilenceMeasuresNewestAcceptedFrame(t *testing.T) {
	origin := time.Now()
	monitor := NewMonitor(origin)
	if got := monitor.Silence(origin.Add(3 * time.Second)); got != 3*time.Second {
		t.Fatalf("silence before evidence = %v", got)
	}
	monitor.Observe(origin.Add(5 * time.Second))
	monitor.Observe(origin.Add(2 * time.Second))
	if got := monitor.Silence(origin.Add(7 * time.Second)); got != 2*time.Second {
		t.Fatalf("stale observation moved evidence backwards: %v", got)
	}
}

func TestWatchDefersExpiryWhileFramesArrive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		monitor := NewMonitor(time.Now())
		done := make(chan error, 1)
		var silence time.Duration
		go func() {
			var err error
			silence, err = Watch(t.Context(), monitor, 3*time.Second)
			done <- err
		}()
		for range 5 {
			time.Sleep(2 * time.Second)
			monitor.Observe(time.Now())
		}
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("expired while frames arrived: %v", err)
		default:
		}
		time.Sleep(3 * time.Second)
		if err := <-done; !errors.Is(err, ErrSilent) || silence != 3*time.Second {
			t.Fatalf("err=%v silence=%v", err, silence)
		}
	})
}

func TestWatchStopsWithItsOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := Watch(ctx, NewMonitor(time.Now()), time.Minute)
			done <- err
		}()
		time.Sleep(time.Second)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}
