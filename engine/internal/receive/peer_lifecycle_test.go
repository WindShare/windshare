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

func TestReceiverPeerGenerationContextIsolatesTaskCauseAndSessionClose(t *testing.T) {
	for _, test := range []struct {
		name         string
		cancelParent func(taskStop context.CancelCauseFunc, continuationStop context.CancelFunc)
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
			cancelParent: func(_ context.CancelCauseFunc, continuationStop context.CancelFunc) {
				continuationStop()
			},
			closeReason: ReceiverLocalStopNormalCompletion,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			taskCtx, stopTask := context.WithCancelCause(context.Background())
			defer stopTask(nil)
			continuationCtx, closeContinuation := context.WithCancel(taskCtx)
			defer closeContinuation()
			// Mirror getReceiverRecovery.open: the generation's detached resource
			// context owns the relay and peer lifetimes independently of task/continuation.
			generationCtx, cancelGeneration := context.WithCancel(context.WithoutCancel(continuationCtx))
			defer cancelGeneration()

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
			var generationLiveAtClose bool
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
								generationLiveAtClose = generationCtx.Err() == nil
								source.terminal <- v2peer.ReceiverTerminationTrace{}
							},
						}, nil
					},
				}, nil
			}}

			peer := worker.startReceiverPeer(generationCtx, nil, observation, nil, stop, receiverPeerPreferred)
			if peer == nil || peerCtx == nil {
				t.Fatal("startReceiverPeer returned no active peer")
			}

			// Neither task cancellation nor continuation shutdown cancels the peer
			// directly; CloseWithReason owns both stop attribution and teardown.
			test.cancelParent(stopTask, closeContinuation)
			if peerCtx.Err() != nil {
				t.Fatalf("parent cancellation leaked into peer context: err=%v cause=%v", peerCtx.Err(), context.Cause(peerCtx))
			}
			if got := stop.snapshot(); got != ReceiverLocalStopNone {
				t.Fatalf("stop recorded before CloseWithReason: %v", got)
			}

			peer.CloseWithReason(test.closeReason)
			cancelGeneration()
			observation.complete()

			if reasonAtClose != test.closeReason {
				t.Fatalf("stop reason at attempt.Close = %v, want %v", reasonAtClose, test.closeReason)
			}
			if causeAtClose != context.Canceled {
				t.Fatalf("peer context cause at close = %v, want plain context.Canceled", causeAtClose)
			}
			if !generationLiveAtClose {
				t.Fatal("generation resource context closed before peer teardown")
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

func TestReceiverPeerInheritsGenerationContextCancellationAsFallback(t *testing.T) {
	generationCtx, cancelGeneration := context.WithCancel(context.Background())
	var peerCtx context.Context
	worker := &runner{receiverPeerFactory: func() (receiverPeerStarter, error) {
		return lifecycleTestPeerStarter{
			start: func(ctx context.Context, _ *receiversession.Runtime) (receiverPeerAttempt, error) {
				peerCtx = ctx
				return newCLIReceiverPeerAttempt(), nil
			},
		}, nil
	}}
	observation, _ := newTestObservation(t)
	peer := worker.startReceiverPeer(generationCtx, nil, observation, nil, &receiverLocalStop{}, receiverPeerPreferred)
	if peer == nil || peerCtx == nil {
		t.Fatal("startReceiverPeer returned no active peer")
	}
	defer peer.Close()
	cancelGeneration()
	if peerCtx.Err() != context.Canceled {
		t.Fatalf("peer context did not inherit generation close: %v", peerCtx.Err())
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

func TestReceiverSessionOpenSeparatesWaitCancellationFromSessionContext(t *testing.T) {
	recovery := &getReceiverRecovery{}

	t.Run("pre-canceled context skips connect", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(task.ErrCancelled)
		called := false
		_, err := recovery.open(ctx, func(context.Context, context.Context, relayset.ReceiverConfig) (*relayset.Receiver, error) {
			called = true
			return nil, nil
		})
		if called || !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled open called=%v err=%v", called, err)
		}
	})

	t.Run("in-flight wait cancel stops connect and cancels detached sessionCtx on failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			entered := make(chan struct{})
			var seenSessionCtx context.Context
			done := make(chan error, 1)
			go func() {
				_, err := recovery.open(ctx, func(waitCtx, sessionCtx context.Context, _ relayset.ReceiverConfig) (*relayset.Receiver, error) {
					seenSessionCtx = sessionCtx
					close(entered)
					<-waitCtx.Done()
					if sessionCtx.Err() != nil {
						t.Errorf("wait cancel leaked into detached sessionCtx: %v", sessionCtx.Err())
					}
					return nil, waitCtx.Err()
				})
				done <- err
			}()
			<-entered
			cancel(task.ErrCancelled)
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("in-flight cancel err=%v, want context.Canceled", err)
			}
			if seenSessionCtx == nil || context.Cause(seenSessionCtx) != context.Canceled {
				t.Fatalf("session context after failed open = %v, want plain context.Canceled", context.Cause(seenSessionCtx))
			}
		})
	})

	t.Run("session Close invokes lifetime cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		session := &getReceiverSession{ctx: ctx, cancel: cancel}
		session.Close()
		if ctx.Err() != context.Canceled {
			t.Fatalf("session Close did not cancel session lifetime: %v", ctx.Err())
		}
	})
}

func TestEarlyExitStopReasonPreservesFailureOwnership(t *testing.T) {
	networkErr := errors.New("session lost during shape probe")
	admissionErr := errors.New("resume admission failed")
	for _, test := range []struct {
		name          string
		contextErr    error
		admissionErr  error
		runtimeErr    error
		connectionErr error
		step          stepOutcome
		want          ReceiverLocalStopReason
	}{
		{"caller canceled", context.Canceled, nil, nil, nil, stepLocalFailure, ReceiverLocalStopCaller},
		{"network preparation failure", nil, nil, nil, nil, stepNetworkFailure, ReceiverLocalStopRuntimeSessionFailure},
		{"runtime failure during local preparation", nil, nil, networkErr, nil, stepLocalFailure, ReceiverLocalStopRuntimeSessionFailure},
		{"connection failure during local preparation", nil, nil, nil, networkErr, stepLocalFailure, ReceiverLocalStopRuntimeSessionFailure},
		{"admission failure", nil, admissionErr, nil, nil, stepLocalFailure, ReceiverLocalStopOutputAdmission},
		{"local output preparation failure", nil, nil, nil, nil, stepLocalFailure, ReceiverLocalStopOutputAdmission},
		{"invalid request failure", nil, nil, nil, nil, stepInvalidRequest, ReceiverLocalStopCaller},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := earlyExitStopReason(test.contextErr, test.admissionErr, test.runtimeErr, test.connectionErr, test.step); got != test.want {
				t.Fatalf("earlyExitStopReason = %v, want %v", got, test.want)
			}
		})
	}
}
