package v2peer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

type controlledAdmissionSession struct {
	*testPeerSession
	entered           chan struct{}
	release           chan struct{}
	contextCanceled   chan struct{}
	contextCancelOnce sync.Once
}

func (session *controlledAdmissionSession) AdmitPeerChannel(
	ctx context.Context,
	channel protocolsession.FrameChannel,
) (sessionruntime.LaneIdentity, error) {
	session.admissions <- channel
	close(session.entered)
	go func() {
		<-ctx.Done()
		session.contextCancelOnce.Do(func() { close(session.contextCanceled) })
	}()
	<-session.release
	// This adversarial implementation deliberately returns a real admission
	// after observing cancellation. The sender must respect the core success
	// boundary instead of rewriting it as the competing transport terminal.
	return session.lane, nil
}

type admissionBoundaryResult struct {
	done bool
	err  error
}

type admissionBoundaryHarness struct {
	attempt         *peerAttempt
	execution       *attemptExecution
	session         *controlledAdmissionSession
	collector       *senderObservationCollector
	cancelExecution context.CancelCauseFunc
	timeout         chan time.Time
	admissionDone   chan struct{}
}

func TestSuccessfulAdmissionWinsEveryCompetingTerminalBoundary(t *testing.T) {
	boundaryErr := errors.New("synthetic boundary failure")
	for _, test := range []struct {
		name          string
		start         func(*admissionBoundaryHarness) <-chan admissionBoundaryResult
		closeAfterWin bool
		lifetimeStops bool
		wantDone      bool
		wantErr       error
	}{
		{
			name: "runtime cancellation",
			start: func(harness *admissionBoundaryHarness) <-chan admissionBoundaryResult {
				result := make(chan admissionBoundaryResult, 1)
				go func() {
					result <- admissionBoundaryResult{done: true, err: harness.execution.runEvents()}
				}()
				harness.cancelExecution(boundaryErr)
				return result
			},
			lifetimeStops: true,
			wantDone:      true,
			wantErr:       boundaryErr,
		},
		{
			name: "admission timeout",
			start: func(harness *admissionBoundaryHarness) <-chan admissionBoundaryResult {
				result := make(chan admissionBoundaryResult, 1)
				go func() {
					result <- admissionBoundaryResult{done: true, err: harness.execution.runEvents()}
				}()
				harness.timeout <- time.Unix(1, 0)
				return result
			},
			closeAfterWin: true,
			wantDone:      true,
		},
		{
			name: "connection failure",
			start: func(harness *admissionBoundaryHarness) <-chan admissionBoundaryResult {
				result := make(chan admissionBoundaryResult, 1)
				go func() {
					result <- admissionBoundaryResult{done: true, err: harness.execution.runEvents()}
				}()
				harness.attempt.push(attemptEvent{kind: attemptConnectionFailed, err: boundaryErr})
				return result
			},
			wantDone: true,
			wantErr:  boundaryErr,
		},
		{
			name: "operation cancellation",
			start: func(harness *admissionBoundaryHarness) <-chan admissionBoundaryResult {
				result := make(chan admissionBoundaryResult, 1)
				go func() {
					result <- admissionBoundaryResult{done: true, err: harness.execution.runEvents()}
				}()
				harness.attempt.push(attemptEvent{kind: attemptOperationCanceled})
				return result
			},
			closeAfterWin: true,
			wantDone:      true,
		},
		{
			name: "candidate failure outside the terminal allow-list",
			start: func(harness *admissionBoundaryHarness) <-chan admissionBoundaryResult {
				result := make(chan admissionBoundaryResult, 1)
				harness.execution.remoteCandidates = harness.attempt.config.factory.maxCandidates
				go func() {
					result <- admissionBoundaryResult{done: true, err: harness.execution.runEvents()}
				}()
				harness.attempt.push(attemptEvent{
					kind:      attemptRemoteCandidate,
					candidate: v2signal.Candidate{Binding: harness.attempt.binding()},
				})
				return result
			},
			wantDone: true,
			wantErr:  errCandidateLimit,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newAdmissionBoundaryHarness(t)
			result := test.start(harness)

			// The competitor has reached the arbiter and canceled only the
			// in-flight admission context. Releasing after this acknowledgement
			// makes the boundary order deterministic instead of scheduler-timed.
			receiveTest(t, harness.session.contextCanceled)
			close(harness.session.release)
			receiveTest(t, harness.admissionDone)
			waitForTest(t, func() bool { return harness.attempt.attached.Load() })
			if stopped := harness.execution.ctx.Err() != nil; stopped != test.lifetimeStops {
				t.Fatalf("attempt lifetime stopped=%t, want %t", stopped, test.lifetimeStops)
			}
			if test.closeAfterWin {
				harness.attempt.push(attemptEvent{kind: attemptChannelDone})
			}

			actual := receiveTest(t, result)
			if actual.done != test.wantDone || !errors.Is(actual.err, test.wantErr) {
				t.Fatalf("boundary result = %#v, want done=%t err=%v", actual, test.wantDone, test.wantErr)
			}
			waitForTest(t, func() bool {
				return len(harness.collector.forAttempt(harness.attempt.binding().AttemptID)) == 7
			})
			observations := harness.collector.forAttempt(harness.attempt.binding().AttemptID)
			if len(observations) != 7 || observations[len(observations)-1].Stage != SenderAttemptAdmitted {
				t.Fatalf("admission terminal was overwritten: %#v", observations)
			}
			harness.attempt.finish(
				context.Background(), actual.err, nil, harness.execution.operationCanceled,
			)
			if observations = harness.collector.forAttempt(harness.attempt.binding().AttemptID); len(observations) != 7 {
				t.Fatalf("finish appended a post-admission terminal: %#v", observations)
			}
			select {
			case failure := <-harness.session.failures:
				t.Fatalf("admitted boundary emitted operation failure %#v", failure)
			default:
			}
		})
	}
}

func newAdmissionBoundaryHarness(t *testing.T) *admissionBoundaryHarness {
	t.Helper()
	collector := &senderObservationCollector{}
	peer := newTestPeerConnection()
	factory := mustTestFactory(t, Config{
		Observer: SenderAttemptObserverFunc(collector.observe),
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
	})
	session := &controlledAdmissionSession{
		testPeerSession: newTestPeerSession(128),
		entered:         make(chan struct{}),
		release:         make(chan struct{}),
		contextCanceled: make(chan struct{}),
	}
	binding := testBinding(129)
	attempt := newPeerAttempt(peerAttemptConfig{
		factory: factory,
		session: session,
		offer:   v2signalOffer(binding),
	})
	attempt.recorder.begin()
	attempt.recorder.complete(SenderAttemptAnswerCreated, SenderCandidateCounts{}, nil, nil)
	attempt.recorder.complete(SenderAttemptAnswerSent, SenderCandidateCounts{}, nil, nil)

	executionContext, cancelExecution := context.WithCancelCause(context.Background())
	attempt.cancelMu.Lock()
	attempt.cancel = cancelExecution
	attempt.cancelMu.Unlock()
	execution := newAttemptExecution(attempt, executionContext, peer)
	timeout := make(chan time.Time)
	execution.timeout = timeout
	channel := newTestPeerChannel()
	execution.channel = channel
	admissionDone := make(chan struct{})
	go func() {
		defer close(admissionDone)
		attempt.awaitOpenAndAdmit(executionContext, channel)
	}()
	receiveTest(t, session.entered)
	return &admissionBoundaryHarness{
		attempt: attempt, execution: execution, session: session, collector: collector,
		cancelExecution: cancelExecution, timeout: timeout, admissionDone: admissionDone,
	}
}

func v2signalOffer(binding v2signal.Binding) v2signal.Offer {
	return v2signal.Offer{Binding: binding, SDP: "v=0\r\n"}
}
