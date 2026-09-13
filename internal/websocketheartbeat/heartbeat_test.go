package websocketheartbeat

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

type pingFunc func(context.Context) error

func (f pingFunc) Ping(ctx context.Context) error { return f(ctx) }

func TestProbeDeadlineAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var events []Event
		var calls int
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, pingFunc(func(probe context.Context) error {
				calls++
				<-probe.Done()
				return probe.Err()
			}), Config{}, func(event Event) { events = append(events, event) })
		}()
		synctest.Wait()
		time.Sleep(DefaultInterval)
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("probes = %d", calls)
		}
		time.Sleep(DefaultTimeout - time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("early failure: %v", err)
		default:
		}
		time.Sleep(time.Nanosecond)
		err := <-done
		if !errors.Is(err, ErrFailed) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if calls != 1 || len(events) != 2 || events[1].Stage != Failed || events[1].Elapsed != DefaultTimeout {
			t.Fatalf("calls=%d events=%+v", calls, events)
		}
	})
}

func TestHealthyIdleSocketAndBoundedPressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		events := make(chan Event, 4)
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, pingFunc(func(context.Context) error {
				time.Sleep(2 * DefaultInterval)
				return nil
			}), Config{}, func(event Event) { events <- event })
		}()
		if probe := <-events; probe.Stage != Probe {
			t.Fatalf("%+v", probe)
		}
		if ack := <-events; ack.Stage != Acknowledged || ack.Elapsed != 2*DefaultInterval {
			t.Fatalf("%+v", ack)
		}
		if probe := <-events; probe.Stage != Probe || probe.Round != 2 {
			t.Fatalf("%+v", probe)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestConfigurationAndCancellation(t *testing.T) {
	for _, config := range []Config{{Interval: -1}, {Timeout: -1}} {
		if _, err := config.Normalize(); !errors.Is(err, ErrConfig) {
			t.Fatal(err)
		}
		if err := Run(context.Background(), pingFunc(nil), config, nil); !errors.Is(err, ErrConfig) {
			t.Fatal(err)
		}
	}
	if err := Run(context.Background(), nil, Config{}, nil); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, pingFunc(func(context.Context) error { t.Fatal("canceled probe"); return nil }), Config{Interval: time.Hour, Timeout: time.Hour}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
