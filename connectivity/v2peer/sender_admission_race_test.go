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
	rejection         *protocolsession.LaneRejection
}

func (session *controlledAdmissionSession) AdmitPeerChannel(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	control sessionruntime.SenderPeerAdmissionControl,
) (sessionruntime.SenderPeerAdmissionResult, error) {
	session.admissions <- channel
	grantOperation := testOperationID(248)
	if !control.BeginAuthenticatedSettlement(grantOperation, session.lane) {
		_ = channel.Close()
		return sessionruntime.SenderPeerAdmissionResult{
			Disposition:      sessionruntime.SenderPeerAdmissionSilentClose,
			ResponseDelivery: sessionruntime.SenderPeerResponseNotAttempted,
			LaneAttachment:   sessionruntime.SenderPeerLaneAttachmentNotAttempted,
		}, context.Canceled
	}
	close(session.entered)
	go func() {
		<-ctx.Done()
		session.contextCancelOnce.Do(func() { close(session.contextCanceled) })
	}()
	<-session.release
	if session.rejection != nil {
		_ = channel.Close()
		return sessionruntime.SenderPeerAdmissionResult{
			SettlementBegan: true, GrantOperationID: grantOperation, Lane: session.lane,
			Disposition:      sessionruntime.SenderPeerAdmissionRejected,
			Rejection:        *session.rejection,
			ResponseDelivery: sessionruntime.SenderPeerResponseDelivered,
			LaneAttachment:   sessionruntime.SenderPeerLaneAttachmentNotAttempted,
		}, &sessionruntime.LaneRejectedError{Rejection: *session.rejection}
	}
	return sessionruntime.SenderPeerAdmissionResult{
		SettlementBegan: true, GrantOperationID: grantOperation, Lane: session.lane,
		Disposition:      sessionruntime.SenderPeerAdmissionAccepted,
		ResponseDelivery: sessionruntime.SenderPeerResponseDelivered,
		LaneAttachment:   sessionruntime.SenderPeerLaneAttached,
	}, nil
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
	expireAdmission func()
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
		alternateErr  error
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
			alternateErr:  boundaryErr,
		},
		{
			name: "admission timeout",
			start: func(harness *admissionBoundaryHarness) <-chan admissionBoundaryResult {
				result := make(chan admissionBoundaryResult, 1)
				go func() {
					result <- admissionBoundaryResult{done: true, err: harness.execution.runEvents()}
				}()
				harness.expireAdmission()
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
			matchesError := errors.Is(actual.err, test.wantErr) ||
				(test.alternateErr != nil && errors.Is(actual.err, test.alternateErr))
			if actual.done != test.wantDone || !matchesError {
				t.Fatalf("boundary result = %#v, want done=%t err=%v", actual, test.wantDone, test.wantErr)
			}
			waitForTest(t, func() bool {
				return senderAttemptReachedTerminal(
					harness.collector.forAttempt(harness.attempt.binding().AttemptID),
					SenderAttemptAdmitted,
				)
			})
			observations := harness.collector.forAttempt(harness.attempt.binding().AttemptID)
			wantStages := successfulSenderAttemptStages
			if len(observations) == len(successfulSenderAttemptStages)+1 {
				wantStages = append(
					append([]SenderAttemptStage{}, successfulSenderAttemptStages[:8]...),
					append([]SenderAttemptStage{SenderAttemptAdmissionDeadlineExpired}, successfulSenderAttemptStages[8:]...)...,
				)
			}
			assertSenderAttemptStages(t, observations, wantStages)
			if observations[len(observations)-1].Stage != SenderAttemptAdmitted {
				t.Fatalf("admission terminal was overwritten: %#v", observations)
			}
			observedCount := len(observations)
			harness.attempt.finish(
				context.Background(), actual.err, nil, harness.execution.operationCanceled,
			)
			if observations = harness.collector.forAttempt(harness.attempt.binding().AttemptID); len(observations) != observedCount {
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

func TestAuthenticatedLaneRejectionWinsAdmissionTimeout(t *testing.T) {
	rejection := protocolsession.LaneRejection{
		Code:       protocolsession.LaneRejectAdmissionLimited,
		RetryAfter: 7 * time.Second,
	}
	harness := newAdmissionBoundaryHarnessWithRejection(t, &rejection)
	result := make(chan error, 1)
	go func() { result <- harness.execution.runEvents() }()
	harness.expireAdmission()
	receiveTest(t, harness.session.contextCanceled)
	close(harness.session.release)
	receiveTest(t, harness.admissionDone)
	actual := receiveTest(t, result)
	var rejected *sessionruntime.LaneRejectedError
	if !errors.As(actual, &rejected) || rejected.Rejection != rejection {
		t.Fatalf("authenticated rejection = %#v, error=%v", rejected, actual)
	}
	if errors.Is(actual, ErrPeerAdmissionTimeout) {
		t.Fatalf("admission timeout hid authenticated rejection: %v", actual)
	}
	if harness.attempt.attached.Load() {
		t.Fatal("rejected admission published a lane")
	}
	peer := harness.execution.peer.(*testPeerConnection)
	receiveTest(t, peer.closed)
	if cleanup := harness.execution.close(actual); cleanup != nil {
		t.Fatalf("post-rejection cleanup = %v", cleanup)
	}
	channel := harness.execution.transport.PeerDataChannel.(*testPeerChannel)
	if peer.closeCalls.Load() != 1 || channel.closeCalls.Load() != 1 {
		t.Fatalf("transport close calls peer=%d channel=%d", peer.closeCalls.Load(), channel.closeCalls.Load())
	}
	select {
	case failure := <-harness.session.failures:
		t.Fatalf("authenticated lane rejection emitted peer operation failure %#v", failure)
	default:
	}
}

func newAdmissionBoundaryHarness(t *testing.T) *admissionBoundaryHarness {
	return newAdmissionBoundaryHarnessWithRejection(t, nil)
}

func newAdmissionBoundaryHarnessWithRejection(
	t *testing.T,
	rejection *protocolsession.LaneRejection,
) *admissionBoundaryHarness {
	t.Helper()
	collector := &senderObservationCollector{}
	peer := newTestPeerConnection()
	factory := mustTestFactoryWithSenderCollector(t, collector, Config{
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
	})
	session := &controlledAdmissionSession{
		testPeerSession: newTestPeerSession(128),
		entered:         make(chan struct{}),
		release:         make(chan struct{}),
		contextCanceled: make(chan struct{}),
		rejection:       rejection,
	}
	binding := testBinding(129)
	attempt := newPeerAttempt(peerAttemptConfig{
		factory: factory,
		session: session,
		offer:   v2signalOffer(binding),
	})
	attempt.recorder.begin()
	attempt.recorder.negotiationDeadlineArmed()
	attempt.recorder.complete(SenderAttemptAnswerCreated, SenderCandidateCounts{}, nil, nil)
	attempt.recorder.complete(SenderAttemptAnswerSent, SenderCandidateCounts{}, nil, nil)

	executionContext, cancelExecution := context.WithCancelCause(context.Background())
	attempt.cancelMu.Lock()
	attempt.cancel = cancelExecution
	attempt.cancelMu.Unlock()
	execution := newAttemptExecution(attempt, executionContext, peer)
	phaseContext, err := attempt.phases.beginNegotiation(executionContext)
	if err != nil {
		t.Fatal(err)
	}
	execution.phaseContext = phaseContext
	channel := newTestPeerChannel()
	transport := newOwnedPeerDataChannel(peer, channel)
	execution.channel = transport
	execution.transport = transport
	openTransition := make(chan struct{})
	execution.openTransition = openTransition
	admissionDone := make(chan struct{})
	go func() {
		defer close(admissionDone)
		attempt.awaitOpenAndAdmit(executionContext, transport, openTransition)
	}()
	receiveTest(t, session.entered)
	expireAdmission := func() {
		attempt.phases.mu.Lock()
		expiration := peerPhaseExpiration{
			phase: PeerAttemptPhaseAdmission, generation: attempt.phases.generation,
			cause: ErrPeerAdmissionTimeout,
		}
		deadline := attempt.phases.deadline
		attempt.phases.mu.Unlock()
		attempt.phases.deadlineFired(expiration)
		deadline.cancel(ErrPeerAdmissionTimeout)
		attempt.phases.expirations <- expiration
	}
	return &admissionBoundaryHarness{
		attempt: attempt, execution: execution, session: session, collector: collector,
		cancelExecution: cancelExecution, expireAdmission: expireAdmission,
		admissionDone: admissionDone,
	}
}

func v2signalOffer(binding v2signal.Binding) v2signal.Offer {
	return v2signal.Offer{Binding: binding, SDP: "v=0\r\n"}
}
