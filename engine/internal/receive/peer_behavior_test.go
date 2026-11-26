package receive

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	receiversession "github.com/windshare/windshare/core/session/receiversession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"github.com/windshare/windshare/engine/internal/task"
)

func TestReceiverPeerDispositionOwnsSessionClosure(t *testing.T) {
	retained := errors.Join(context.Canceled, errors.New("peer shutdown failed"))
	for _, test := range []struct {
		name        string
		disposition receiverPeerDisposition
		signal      receiverPeerSignal
		closed      int32
		local       bool
		warning     bool
	}{
		{"path failure", receiverPeerFallbackAllowed, receiverPeerFailed, 0, false, false},
		{"session unavailable", receiverPeerSessionUnavailable, receiverPeerRuntimeTerminal, 0, false, true},
		{"session unsafe", receiverPeerSessionUnsafe, receiverPeerSessionFatal, 1, false, true},
		{"local stop with residue", receiverPeerLocalStop, 0, 0, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation, events := newTestObservation(t)
			attempt := newCLIReceiverPeerAttempt()
			runtime := &testRuntimeCloser{}
			attempt.finishOutcome(NewPeerOutcome(test.disposition, retained))
			var signals []receiverPeerSignal
			local := (&runner{}).monitorReceiverPeer(attempt, runtime, protocolsession.ProtocolSessionID{1}, observation, func(signal receiverPeerSignal) { signals = append(signals, signal) })
			if local != test.local || runtime.calls.Load() != test.closed {
				t.Fatalf("local=%v closes=%d", local, runtime.calls.Load())
			}
			if test.signal == 0 {
				if len(signals) != 0 {
					t.Fatal(signals)
				}
			} else if len(signals) != 1 || signals[0] != test.signal {
				t.Fatal(signals)
			}
			warnings := warningEvents(*events)
			if (len(warnings) > 0) != test.warning {
				t.Fatalf("warnings=%v", warnings)
			}
			if test.warning && !errors.Is(warnings[0].(Warning).Cause, retained) {
				t.Fatalf("joined failure=%v", warnings[0])
			}
		})
	}
}
func TestReceiverPeerReadyDetachAndCloseJoin(t *testing.T) {
	observation, events := newTestObservation(t)
	attempt := newCLIReceiverPeerAttempt()
	attempt.lane = session.LaneIdentity{ID: 2, Epoch: 1}
	signals := make(chan receiverPeerSignal, 2)
	done := make(chan struct{})
	stop := &receiverLocalStop{}
	peer := &activeReceiverPeer{attempt: attempt, done: done, localStop: stop}
	go func() {
		defer close(done)
		(&runner{}).monitorReceiverPeer(attempt, &testRuntimeCloser{}, protocolsession.ProtocolSessionID{1}, observation, func(signal receiverPeerSignal) { signals <- signal })
	}()
	close(attempt.ready)
	if signal := <-signals; signal != receiverPeerReady {
		t.Fatal(signal)
	}
	attempt.finish(errors.New("direct path lost"))
	if signal := <-signals; signal != receiverPeerDetached {
		t.Fatal(signal)
	}
	peer.CloseWithReason(ReceiverLocalStopNormalCompletion)
	peer.Close()
	if stop.snapshot() != ReceiverLocalStopNormalCompletion {
		t.Fatal(stop.snapshot())
	}
	adopted := 0
	for _, event := range *events {
		if _, ok := event.(LaneAdopted); ok {
			adopted++
		}
	}
	if adopted != 1 {
		t.Fatalf("adopted=%d", adopted)
	}
}
func TestReceiverContentPathsDescribeUsefulDeliveryAndRealFallback(t *testing.T) {
	observation, events := newTestObservation(t)
	paths := newReceiverContentPaths(observation)
	now := time.Unix(100, 0)
	direct := transferlanes.ContentActivity{Route: transferlanes.RouteDirect, AdmittedLanes: 1, UsefulBytes: 10, LastUsefulAt: now}
	relay := transferlanes.ContentActivity{Route: transferlanes.RouteRelay, AdmittedLanes: 1, UsefulBytes: 10, LastUsefulAt: now}
	paths.observeContent([]transferlanes.ContentActivity{direct}, now)
	paths.observeContent([]transferlanes.ContentActivity{direct, relay}, now)
	paths.observeContent([]transferlanes.ContentActivity{relay}, now)
	var selected []ContentPath
	fallbacks := 0
	for _, event := range *events {
		switch value := event.(type) {
		case ContentPathObserved:
			selected = append(selected, value.Path)
		case FallbackObserved:
			fallbacks++
		}
	}
	if len(selected) != 3 || selected[0] != ContentPathDirect || selected[1] != ContentPathDirectAndRelay || selected[2] != ContentPathRelay || fallbacks != 1 {
		t.Fatalf("paths=%v fallback=%d", selected, fallbacks)
	}
	// Idle but admitted peers carry no useful traffic; that cannot prove fallback.
	observation, events = newTestObservation(t)
	paths = newReceiverContentPaths(observation)
	direct.UsefulBytes = 0
	direct.LastUsefulAt = time.Time{}
	paths.observeContent([]transferlanes.ContentActivity{direct, relay}, now)
	paths.observeContent([]transferlanes.ContentActivity{relay}, now)
	for _, event := range *events {
		if _, ok := event.(FallbackObserved); ok {
			t.Fatal("idle peer invented fallback")
		}
	}
}
func TestReceiverPeerFactoryFailurePreservesRequiredAdmission(t *testing.T) {
	observation, events := newTestObservation(t)
	worker := &runner{receiverPeerFactory: func() (receiverPeerStarter, error) { return nil, errors.New("configuration failed") }}
	var signals []receiverPeerSignal
	peer := worker.startReceiverPeer(context.Background(), nil, observation, func(signal receiverPeerSignal) { signals = append(signals, signal) }, &receiverLocalStop{}, receiverPeerRequired)
	if peer != nil || len(signals) != 1 || signals[0] != receiverPeerFailed {
		t.Fatalf("peer=%v signals=%v", peer, signals)
	}
	if warnings := warningEvents(*events); len(warnings) != 1 || warnings[0].(Warning).Code != FailurePeerConfiguration {
		t.Fatalf("warnings=%v", warnings)
	}
}

type capturingPeerStarter struct {
	started func(context.Context) (receiverPeerAttempt, error)
}

func (s capturingPeerStarter) Start(ctx context.Context, _ *receiversession.Runtime) (receiverPeerAttempt, error) {
	return s.started(ctx)
}

func TestStartReceiverPeerIsolatesTaskCancelCause(t *testing.T) {
	t.Run("pre-canceled task cause becomes standard context.Canceled", func(t *testing.T) {
		observation, _ := newTestObservation(t)
		parent, cancel := context.WithCancelCause(context.Background())
		cancel(task.ErrCancelled)

		var observedCtx context.Context
		attempt := newCLIReceiverPeerAttempt()
		worker := &runner{receiverPeerFactory: func() (receiverPeerStarter, error) {
			return capturingPeerStarter{started: func(ctx context.Context) (receiverPeerAttempt, error) {
				observedCtx = ctx
				return attempt, nil
			}}, nil
		}}
		peer := worker.startReceiverPeer(parent, nil, observation, nil, &receiverLocalStop{}, receiverPeerPreferred)
		if peer == nil {
			t.Fatal("expected active peer")
		}
		defer peer.Close()

		if observedCtx == nil || observedCtx.Err() != context.Canceled {
			t.Fatalf("peer ctx err = %v, want context.Canceled", observedCtx.Err())
		}
		if cause := context.Cause(observedCtx); cause != context.Canceled {
			t.Fatalf("peer ctx cause = %v, want standard context.Canceled (leaked task cause)", cause)
		}
	})

	t.Run("mid-run task cancellation forwards standard context.Canceled", func(t *testing.T) {
		observation, _ := newTestObservation(t)
		parent, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)

		var observedCtx context.Context
		attempt := newCLIReceiverPeerAttempt()
		worker := &runner{receiverPeerFactory: func() (receiverPeerStarter, error) {
			return capturingPeerStarter{started: func(ctx context.Context) (receiverPeerAttempt, error) {
				observedCtx = ctx
				return attempt, nil
			}}, nil
		}}
		peer := worker.startReceiverPeer(parent, nil, observation, nil, &receiverLocalStop{}, receiverPeerPreferred)
		if peer == nil {
			t.Fatal("expected active peer")
		}
		defer peer.Close()

		if observedCtx.Err() != nil {
			t.Fatalf("live peer ctx already ended: %v", observedCtx.Err())
		}
		cancel(task.ErrCancelled)
		<-observedCtx.Done()
		if observedCtx.Err() != context.Canceled || context.Cause(observedCtx) != context.Canceled {
			t.Fatalf("peer ctx err=%v cause=%v, want context.Canceled", observedCtx.Err(), context.Cause(observedCtx))
		}
	})

	t.Run("mid-run parent deadline preserves DeadlineExceeded without leaking custom cause", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			observation, _ := newTestObservation(t)
			type ctxKey struct{}
			customDeadlineCause := errors.New("parent timeout cause")
			parent, cancel := context.WithTimeoutCause(
				context.WithValue(context.Background(), ctxKey{}, "peer-trace"),
				50*time.Millisecond,
				customDeadlineCause,
			)
			defer cancel()

			var observedCtx context.Context
			var attemptCtx context.Context
			var cancelAttempt context.CancelCauseFunc
			attempt := newCLIReceiverPeerAttempt()
			worker := &runner{receiverPeerFactory: func() (receiverPeerStarter, error) {
				return capturingPeerStarter{started: func(ctx context.Context) (receiverPeerAttempt, error) {
					observedCtx = ctx
					attemptCtx, cancelAttempt = context.WithCancelCause(ctx)
					return attempt, nil
				}}, nil
			}}
			peer := worker.startReceiverPeer(parent, nil, observation, nil, &receiverLocalStop{}, receiverPeerPreferred)
			if peer == nil {
				t.Fatal("expected active peer")
			}
			defer peer.Close()
			defer cancelAttempt(nil)

			if got := observedCtx.Value(ctxKey{}); got != "peer-trace" {
				t.Fatalf("peer ctx value = %v, want peer-trace", got)
			}
			<-observedCtx.Done()
			<-attemptCtx.Done()
			if observedCtx.Err() != context.DeadlineExceeded || context.Cause(observedCtx) != context.DeadlineExceeded {
				t.Fatalf("peer ctx err=%v cause=%v, want context.DeadlineExceeded", observedCtx.Err(), context.Cause(observedCtx))
			}
			if attemptCtx.Err() != context.DeadlineExceeded || context.Cause(attemptCtx) != context.DeadlineExceeded {
				t.Fatalf("derived attempt ctx err=%v cause=%v, want context.DeadlineExceeded", attemptCtx.Err(), context.Cause(attemptCtx))
			}
		})
	})

	t.Run("child WithCancelCause preserves its own local cause", func(t *testing.T) {
		parent, cancelParent := context.WithCancelCause(context.Background())
		defer cancelParent(nil)
		peerCtx := standardCancellationContext(parent)
		child, cancelChild := context.WithCancelCause(peerCtx)
		localErr := errors.New("peer negotiation timeout")
		cancelChild(localErr)
		if child.Err() != context.Canceled || context.Cause(child) != localErr {
			t.Fatalf("child ctx err=%v cause=%v, want local cause %v", child.Err(), context.Cause(child), localErr)
		}
	})
}
