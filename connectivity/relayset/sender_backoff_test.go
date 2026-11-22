package relayset

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestWakeHonorsServerDeadlineAcrossRecoveryOwners(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failure  error
		deadline time.Duration
	}{
		{"HTTP", &relayv2.RegistrationNotSentError{Cause: &relayv2.WebSocketDialError{StatusCode: http.StatusServiceUnavailable, RetryAfter: 30 * time.Second}}, 30 * time.Second},
		{"WS2E", &relayv2.RelayError{Code: v2.ErrorAccessUnavailable, RetryAfter: time.Second}, time.Second},
		{"bounded HTTP", &relayv2.WebSocketDialError{StatusCode: http.StatusTooManyRequests, RetryAfter: time.Hour}, 2 * senderRelaySlowRetry},
		{"capacity without hint", &relayv2.WebSocketDialError{StatusCode: http.StatusServiceUnavailable}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var attempts atomic.Int32
				dialer := &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (SenderConnection, error) {
					if attempts.Add(1) == 1 {
						return SenderConnection{}, tc.failure
					}
					return NewSenderConnection(&senderRelayTestEndpoint{}), nil
				}}
				l := newSenderRelayTestLifecycle(t, nil, dialer, nil)
				defer l.StopRecovery()
				var failure Attempt
				l.config.ObserveAttempt = func(a Attempt) {
					if a.State == AttemptFailed {
						failure = a
					}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- l.WaitReady(ctx) }()
				synctest.Wait()
				if failure.NextDelay != max(senderRelayRetryInitial, tc.deadline) {
					t.Fatal("retry cadence", failure)
				}
				l.Wake()
				synctest.Wait()
				if tc.deadline > 0 {
					if attempts.Load() != 1 || !failure.ServerRetryAt.Equal(start.Add(tc.deadline)) {
						t.Fatal("wake bypassed deadline", failure)
					}
					cancel()
					if err := <-done; !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
					go func() { done <- l.WaitReady(t.Context()) }()
					synctest.Wait()
					l.Wake()
					time.Sleep(tc.deadline - time.Nanosecond)
					synctest.Wait()
					if attempts.Load() != 1 {
						t.Fatal("new recovery owner bypassed deadline")
					}
					time.Sleep(time.Nanosecond)
				}
				if err := <-done; err != nil || attempts.Load() != 2 || time.Since(start) != tc.deadline {
					t.Fatal(err, attempts.Load(), time.Since(start))
				}
			})
		})
	}
}

type parkedPermit struct{}

func (parkedPermit) Admit() bool { return false }
func (parkedPermit) Release()    {}

type slowRetirement struct{ senderRelayTestEndpoint }

func (*slowRetirement) Close() error { time.Sleep(5 * time.Second); return nil }

func TestParkedSampleMeasuresOnlyTheSuccessfulDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dialer := &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (SenderConnection, error) {
			time.Sleep(130 * time.Millisecond)
			return NewSenderConnection(&slowRetirement{}), nil
		}}
		l := newSenderRelayTestLifecycle(t, nil, dialer, nil)
		defer l.StopRecovery()
		l.config.Attempts = attemptControlFunc(func(context.Context) (AttemptPermit, error) { return parkedPermit{}, nil })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var sample Attempt
		l.config.ObserveAttempt = func(a Attempt) {
			if a.State == AttemptParked {
				sample = a
				cancel()
			}
		}
		if err := l.WaitReady(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if sample.HandshakeDuration != 130*time.Millisecond || sample.Duration != 5*time.Second+sample.HandshakeDuration {
			t.Fatal(sample)
		}
	})
}

func TestRecoveryAdmissionCancellationPreservesCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{{"caller deadline", context.DeadlineExceeded}, {"stopped", ErrStopped}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := newSenderRelayTestLifecycle(t, nil, nil, nil)
				defer l.StopRecovery()
				entered := make(chan struct{})
				l.config.Attempts = attemptControlFunc(func(ctx context.Context) (AttemptPermit, error) {
					close(entered)
					<-ctx.Done()
					return nil, ctx.Err()
				})
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- l.WaitReady(ctx) }()
				<-entered
				if tc.want == ErrStopped {
					l.StopRecovery()
				}
				if err := <-done; !errors.Is(err, tc.want) {
					t.Fatalf("admission returned %v, want %v", err, tc.want)
				}
			})
		})
	}
}

func TestServerDelayCrossingFastWindowAnnouncesWaiting(t *testing.T) {
	for _, hints := range [][]time.Duration{{time.Minute}, {30 * time.Second, 30 * time.Second}} {
		synctest.Test(t, func(t *testing.T) {
			var calls int
			dialer := &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (SenderConnection, error) {
				calls++
				if calls <= len(hints) {
					return SenderConnection{}, &relayv2.WebSocketDialError{StatusCode: 503, RetryAfter: hints[calls-1]}
				}
				return NewSenderConnection(&senderRelayTestEndpoint{}), nil
			}}
			l := newSenderRelayTestLifecycle(t, nil, dialer, nil)
			defer l.StopRecovery()
			var waiting []Attempt
			l.config.ObserveAttempt = func(a Attempt) {
				if a.State == AttemptWaiting {
					waiting = append(waiting, a)
				}
			}
			start := time.Now()
			if err := l.WaitReady(t.Context()); err != nil {
				t.Fatal(err)
			}
			if time.Since(start) != time.Minute || len(waiting) != 1 || !waiting[0].Slow ||
				int(waiting[0].Number) != len(hints) || waiting[0].NextDelay != hints[len(hints)-1] || !l.serverRetryAt.IsZero() {
				t.Fatalf("hints=%v elapsed=%v waiting=%+v deadline=%v", hints, time.Since(start), waiting, l.serverRetryAt)
			}
		})
	}
}
