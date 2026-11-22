package receive

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/v2peer"
	receiversession "github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/engine/internal/task"
)

type lifecycleTestPeerStarter struct {
	*testReceiverObservationSource
	start func(context.Context, *receiversession.Runtime) (receiverPeerAttempt, error)
}

func (starter lifecycleTestPeerStarter) Start(ctx context.Context, runtime *receiversession.Runtime) (receiverPeerAttempt, error) {
	return starter.start(ctx, runtime)
}

type lifecycleTestPeerAttempt struct {
	*cliReceiverPeerAttempt
	onClose func()
}

func (attempt *lifecycleTestPeerAttempt) Close() error {
	if attempt.onClose != nil {
		attempt.onClose()
	}
	return attempt.cliReceiverPeerAttempt.Close()
}

func TestReceiverPeerIsolatesContextFromTaskCauseAndSessionClose(t *testing.T) {
	for _, test := range []struct {
		name         string
		cancelParent func(taskStop context.CancelCauseFunc, sessionStop context.CancelFunc)
		closeReason  ReceiverLocalStopReason
	}{
		{
			name: "caller task cancellation does not leak task cause into peer",
			cancelParent: func(taskStop context.CancelCauseFunc, _ context.CancelFunc) {
				taskStop(task.ErrCancelled)
			},
			closeReason: ReceiverLocalStopCaller,
		},
		{
			name: "continuation session close does not preempt normal completion",
			cancelParent: func(_ context.CancelCauseFunc, sessionStop context.CancelFunc) {
				sessionStop()
			},
			closeReason: ReceiverLocalStopNormalCompletion,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			taskCtx, stopTask := context.WithCancelCause(context.Background())
			defer stopTask(nil)
			sessionCtx, closeSession := context.WithCancel(taskCtx)
			defer closeSession()

			source := &testReceiverObservationSource{
				terminal:   make(chan v2peer.ReceiverTerminationTrace, 1),
				diagnostic: make(chan v2peer.PeerDiagnosticObservation),
			}
			var mu sync.Mutex
			var events []task.Event
			observation := newObservation(task.Control{Now: time.Now, Emit: func(event task.Event) bool {
				mu.Lock()
				events = append(events, event)
				mu.Unlock()
				return true
			}}, true, nil)

			var peerCtx context.Context
			var reasonAtClose ReceiverLocalStopReason
			var causeAtClose error
			stop := &receiverLocalStop{}
			worker := &runner{receiverPeerFactory: func() (receiverPeerStarter, error) {
				return lifecycleTestPeerStarter{
					testReceiverObservationSource: source,
					start: func(ctx context.Context, _ *receiversession.Runtime) (receiverPeerAttempt, error) {
						peerCtx = ctx
						return &lifecycleTestPeerAttempt{
							cliReceiverPeerAttempt: newCLIReceiverPeerAttempt(),
							onClose: func() {
								reasonAtClose = stop.snapshot()
								causeAtClose = context.Cause(peerCtx)
								source.terminal <- v2peer.ReceiverTerminationTrace{}
							},
						}, nil
					},
				}, nil
			}}

			peer := worker.startReceiverPeer(sessionCtx, nil, observation, nil, stop, receiverPeerPreferred)
			if peer == nil || peerCtx == nil {
				t.Fatal("startReceiverPeer returned no active peer")
			}

			// Neither task cancellation nor continuation shutdown cancels the peer
			// directly; CloseWithReason owns both stop attribution and teardown.
			test.cancelParent(stopTask, closeSession)
			if peerCtx.Err() != nil {
				t.Fatalf("parent cancellation leaked into peer context: err=%v cause=%v", peerCtx.Err(), context.Cause(peerCtx))
			}
			if got := stop.snapshot(); got != ReceiverLocalStopNone {
				t.Fatalf("stop recorded before CloseWithReason: %v", got)
			}

			peer.CloseWithReason(test.closeReason)
			observation.complete()

			if reasonAtClose != test.closeReason {
				t.Fatalf("stop reason at attempt.Close = %v, want %v", reasonAtClose, test.closeReason)
			}
			if causeAtClose != context.Canceled {
				t.Fatalf("peer context cause at close = %v, want plain context.Canceled", causeAtClose)
			}
			mu.Lock()
			defer mu.Unlock()
			var terminated []PeerTerminated
			for _, event := range events {
				if value, ok := event.(PeerTerminated); ok {
					terminated = append(terminated, value)
				}
			}
			if len(terminated) != 1 || terminated[0].LocalStop != test.closeReason {
				t.Fatalf("peer terminations = %+v, want one with %v", terminated, test.closeReason)
			}
		})
	}
}

func TestReceiverPeerStartFailureCancelsPeerContext(t *testing.T) {
	var peerCtx context.Context
	worker := &runner{receiverPeerFactory: func() (receiverPeerStarter, error) {
		return lifecycleTestPeerStarter{
			start: func(ctx context.Context, _ *receiversession.Runtime) (receiverPeerAttempt, error) {
				peerCtx = ctx
				return nil, errors.New("start failed")
			},
		}, nil
	}}
	observation, _ := newTestObservation(t)
	if peer := worker.startReceiverPeer(context.Background(), nil, observation, nil, &receiverLocalStop{}, receiverPeerPreferred); peer != nil {
		t.Fatalf("failed start returned peer=%+v", peer)
	}
	if peerCtx == nil || peerCtx.Err() != context.Canceled {
		t.Fatalf("peer context after failed start = %v, want context.Canceled", peerCtx.Err())
	}
}

func TestReceiverSessionOpenRespectsJoinCancellationWithoutLeakingCause(t *testing.T) {
	recovery := &getReceiverRecovery{}

	t.Run("pre-canceled context skips connect", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(task.ErrCancelled)
		called := false
		_, err := recovery.open(ctx, func(context.Context, relayset.ReceiverConfig) (*relayset.Receiver, error) {
			called = true
			return nil, nil
		})
		if called || !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled open called=%v err=%v", called, err)
		}
	})

	t.Run("in-flight cancel stops join with plain canceled session context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			entered := make(chan struct{})
			var seenCause error
			done := make(chan error, 1)
			go func() {
				_, err := recovery.open(ctx, func(sessionCtx context.Context, _ relayset.ReceiverConfig) (*relayset.Receiver, error) {
					close(entered)
					<-sessionCtx.Done()
					seenCause = context.Cause(sessionCtx)
					return nil, sessionCtx.Err()
				})
				done <- err
			}()
			<-entered
			cancel(task.ErrCancelled)
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("in-flight cancel err=%v, want context.Canceled", err)
			}
			if seenCause != context.Canceled {
				t.Fatalf("session context cause=%v, want plain context.Canceled", seenCause)
			}
		})
	})

	t.Run("session Close invokes lifetime cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		session := &getReceiverSession{cancel: cancel}
		session.Close()
		if ctx.Err() != context.Canceled {
			t.Fatalf("session Close did not cancel session lifetime: %v", ctx.Err())
		}
	})
}

func TestEarlyExitStopReasonPreservesFailureOwnership(t *testing.T) {
	for _, test := range []struct {
		name       string
		contextErr error
		step       stepOutcome
		want       ReceiverLocalStopReason
	}{
		{"caller canceled", context.Canceled, stepLocalFailure, ReceiverLocalStopCaller},
		{"network preparation failure", nil, stepNetworkFailure, ReceiverLocalStopRuntimeSessionFailure},
		{"local preparation failure", nil, stepLocalFailure, ReceiverLocalStopOutputAdmission},
		{"invalid request failure", nil, stepInvalidRequest, ReceiverLocalStopOutputAdmission},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := earlyExitStopReason(test.contextErr, test.step); got != test.want {
				t.Fatalf("earlyExitStopReason(%v, %v) = %v, want %v", test.contextErr, test.step, got, test.want)
			}
		})
	}
}
