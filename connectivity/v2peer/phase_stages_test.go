package v2peer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/turnsession"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestRecoveryReservationUsesEffectiveProviderPhases(t *testing.T) {
	injected := ReceiverPeerConnectionFactoryFunc(func(config pion.Configuration) (ReceiverPeerConnection, error) {
		return pion.NewPeerConnection(config)
	})
	for _, tc := range []struct {
		name   string
		config ReceiverFactoryConfig
		want   time.Duration
	}{
		{"native default", ReceiverFactoryConfig{}, 77 * time.Second},
		{"native staged", ReceiverFactoryConfig{NegotiationBudget: time.Second, AdmissionBudget: MaximumPeerAdmissionBudget}, 82 * time.Second},
		{"injected negotiation", ReceiverFactoryConfig{PeerConnections: injected, NegotiationBudget: 60 * time.Second, AdmissionBudget: 24 * time.Second}, 84 * time.Second},
		{"outer limit", ReceiverFactoryConfig{PeerConnections: injected, NegotiationBudget: time.Duration(1<<63 - 1)}, 85 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory, err := newTestReceiverFactory(t, tc.config)
			if err != nil {
				t.Fatal(err)
			}
			if got := factory.AttemptBudget(85 * time.Second); got != tc.want {
				t.Fatalf("attempt budget = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestNativeObservedStagesPreserveCheckingWindowAndCancelOriginalIO(t *testing.T) {
	timers := newRecordingReceiverPhaseTimerSource()
	lifecycle := newPeerPhaseLifecycle(timers, DefaultPeerNegotiationBudget, DefaultPeerAdmissionBudget)
	lifecycle.staged = true
	ctx, err := lifecycle.beginNegotiation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.terminate(context.Canceled)
	preparation := receiveTest(t, timers.created)
	if preparation.phase != PeerAttemptPhasePreparation || preparation.duration != 10*time.Second {
		t.Fatal(preparation)
	}
	lifecycle.observeICE(pion.ICEConnectionStateChecking)
	checking := receiveTest(t, timers.created)
	if checking.phase != PeerAttemptPhaseChecking || checking.duration != 32*time.Second {
		t.Fatal(checking)
	}
	preparation.timer.Fire()
	if ctx.Err() != nil {
		t.Fatal("retired preparation deadline canceled ICE")
	}
	lifecycle.observeICE(pion.ICEConnectionStateChecking)
	select {
	case extra := <-timers.created:
		t.Fatal("duplicate checking reset deadline", extra)
	default:
	}
	checking.timer.Fire()
	receiveTest(t, ctx.Done())
	if !errors.Is(context.Cause(ctx), ErrPeerNegotiationTimeout) {
		t.Fatal(context.Cause(ctx))
	}
	lifecycle.observeICE(pion.ICEConnectionStateConnected)
	select {
	case extra := <-timers.created:
		t.Fatal("late state resurrected expired attempt", extra)
	default:
	}
}
func TestNativeEstablishedPairAdvancesToDataChannelThenAuthenticatedAdmission(t *testing.T) {
	timers := newRecordingReceiverPhaseTimerSource()
	lifecycle := newPeerPhaseLifecycle(timers, DefaultPeerNegotiationBudget, DefaultPeerAdmissionBudget)
	lifecycle.staged = true
	ctx, err := lifecycle.beginNegotiation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.terminate(context.Canceled)
	receiveTest(t, timers.created)
	lifecycle.observeICE(pion.ICEConnectionStateChecking)
	checking := receiveTest(t, timers.created)
	lifecycle.observeICE(pion.ICEConnectionStateConnected)
	establishment := receiveTest(t, timers.created)
	if establishment.phase != PeerAttemptPhaseEstablishing || establishment.duration != 15*time.Second {
		t.Fatal(establishment)
	}
	checking.timer.Fire()
	if ctx.Err() != nil {
		t.Fatal("successful checks retained timeout")
	}
	lifecycle.observeICE(pion.ICEConnectionStateCompleted)
	select {
	case extra := <-timers.created:
		t.Fatal("ICE completed reset establishment", extra)
	default:
	}
	admission, started, err := lifecycle.beginAdmission(context.Background())
	if err != nil || !started {
		t.Fatal(err)
	}
	phase := receiveTest(t, timers.created)
	if phase.duration != 20*time.Second || phase.phase != PeerAttemptPhaseAdmission {
		t.Fatal(phase)
	}
	phase.timer.Fire()
	receiveTest(t, admission.Done())
	if !errors.Is(context.Cause(admission), ErrPeerAdmissionTimeout) {
		t.Fatal(context.Cause(admission))
	}
}

func TestSenderStagedEstablishingReconcilesOpenedChannelBeforeTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		timers := newRecordingReceiverPhaseTimerSource()
		collector := &senderObservationCollector{}
		peer := newTestPeerConnection()
		factory := mustTestFactoryWithSenderCollector(t, collector, Config{
			PhaseTimers: timers,
			PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
				return peer, nil
			}),
		})
		session := newTestPeerSession(141)
		binding := testBinding(142)
		attempt := newPeerAttempt(peerAttemptConfig{
			factory: factory, session: session, offer: v2signalOffer(binding),
		})
		attempt.phases.staged = true
		attempt.recorder.begin()
		attempt.recorder.negotiationDeadlineArmed()
		attempt.recorder.complete(SenderAttemptAnswerCreated, SenderCandidateCounts{}, nil)
		attempt.recorder.complete(SenderAttemptAnswerSent, SenderCandidateCounts{}, nil)

		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		phaseContext, err := attempt.phases.beginNegotiation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer attempt.phases.terminate(nil)
		receiveTest(t, timers.created) // preparation
		attempt.phases.observeICE(pion.ICEConnectionStateConnected)
		establishment := receiveTest(t, timers.created)
		if establishment.phase != PeerAttemptPhaseEstablishing {
			t.Fatalf("phase = %q, want %q", establishment.phase, PeerAttemptPhaseEstablishing)
		}

		channel := newTestPeerChannel()
		channel.opened = make(chan struct{})
		transport := newOwnedPeerDataChannel(peer, channel)
		execution := newAttemptExecution(attempt, ctx, peer)
		execution.phaseContext = phaseContext
		execution.channel = transport
		execution.transport = transport
		openTransition := make(chan struct{})
		execution.openTransition = openTransition

		// Fire Establishing first so the expiration event is already pending when
		// Opened becomes observable, reproducing the select-order race deterministically.
		establishment.timer.Fire()
		expiration := receiveTest(t, attempt.phases.expirationEvents())
		close(channel.opened)

		type expirationResult struct {
			terminal bool
			err      error
		}
		handled := make(chan expirationResult, 1)
		go func() {
			terminal, handleErr := execution.handlePhaseExpiration(expiration)
			handled <- expirationResult{terminal: terminal, err: handleErr}
		}()
		synctest.Wait()
		select {
		case early := <-handled:
			t.Fatalf("staged Establishing bypassed openTransition reconciliation: %+v", early)
		default:
		}

		admitDone := make(chan struct{})
		go func() {
			defer close(admitDone)
			attempt.awaitOpenAndAdmit(ctx, transport, openTransition)
		}()
		result := receiveTest(t, handled)
		if result.terminal || result.err != nil {
			t.Fatalf("reconciled Establishing expiration terminated attempt: %+v", result)
		}
		admission := receiveTest(t, timers.created)
		if admission.phase != PeerAttemptPhaseAdmission {
			t.Fatalf("next phase = %q, want %q", admission.phase, PeerAttemptPhaseAdmission)
		}
		receiveTest(t, session.admissions)
		receiveTest(t, admitDone)
		for _, event := range collector.forAttempt(binding.AttemptID) {
			if event.Stage == SenderAttemptNegotiationDeadlineExpired {
				t.Fatalf("reconciled Establishing emitted deadline expiration: %#v", event)
			}
		}
	})
}

func TestSenderOpenTransitionUnblocksWhenContextCancelsBeforeConcurrentOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		timers := newRecordingReceiverPhaseTimerSource()
		peer := newTestPeerConnection()
		factory := mustTestFactory(t, Config{PhaseTimers: timers})
		attempt := newPeerAttempt(peerAttemptConfig{
			factory: factory, session: newTestPeerSession(143), offer: v2signalOffer(testBinding(144)),
		})
		attempt.phases.staged = true

		ctx, cancel := context.WithCancelCause(t.Context())
		phaseContext, err := attempt.phases.beginNegotiation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer attempt.phases.terminate(nil)
		receiveTest(t, timers.created) // preparation
		attempt.phases.observeICE(pion.ICEConnectionStateConnected)
		establishment := receiveTest(t, timers.created)

		channel := newTestPeerChannel()
		channel.opened = make(chan struct{})
		transport := newOwnedPeerDataChannel(peer, channel)
		execution := newAttemptExecution(attempt, ctx, peer)
		execution.phaseContext = phaseContext
		execution.channel = transport
		execution.transport = transport
		openTransition := make(chan struct{})
		execution.openTransition = openTransition

		admitDone := make(chan struct{})
		go func() {
			defer close(admitDone)
			attempt.awaitOpenAndAdmit(ctx, transport, openTransition)
		}()
		synctest.Wait()
		cancel(context.Canceled)
		receiveTest(t, admitDone)

		// Even if Opened completes after ctx cancellation and an Establishing
		// expiration is selected first, handlePhaseExpiration must not deadlock.
		establishment.timer.Fire()
		expiration := receiveTest(t, attempt.phases.expirationEvents())
		close(channel.opened)
		terminal, handleErr := execution.handlePhaseExpiration(expiration)
		if terminal || handleErr != nil {
			t.Fatalf("canceled openTransition reconciliation = (%t, %v)", terminal, handleErr)
		}
	})
}

func TestReceiverStagedEstablishingReconcilesOpenedChannelBeforeTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		timers := newRecordingReceiverPhaseTimerSource()
		factory, err := newTestReceiverFactory(t, ReceiverFactoryConfig{PhaseTimers: timers})
		if err != nil {
			t.Fatal(err)
		}
		peer := newReceiverTestPeerConnection()
		channel := newReceiverTestChannel()
		transport := newOwnedPeerDataChannel(peer, channel)
		lanes := newReceiverTestLanes()
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)

		phases := newPeerPhaseLifecycle(timers, factory.negotiationBudget, factory.admissionBudget)
		phases.staged = true
		phaseContext, err := phases.beginNegotiation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer phases.terminate(nil)
		receiveTest(t, timers.created) // preparation
		phases.observeICE(pion.ICEConnectionStateConnected)
		establishment := receiveTest(t, timers.created)
		if establishment.phase != PeerAttemptPhaseEstablishing {
			t.Fatalf("phase = %q, want %q", establishment.phase, PeerAttemptPhaseEstablishing)
		}

		attempt := &ReceiverAttempt{
			releaseTURN: func() {}, factory: factory, lanes: lanes, peer: peer,
			channel: transport, transport: transport, binding: testBinding(145),
			events: make(chan receiverEvent, 8), ctx: ctx, cancel: cancel,
			done: make(chan struct{}), ready: make(chan struct{}),
			phases: phases, phaseContext: phaseContext,
		}
		execution := newReceiverExecution(attempt)
		defer execution.candidateWriter.Close()

		establishment.timer.Fire()
		expiration := receiveTest(t, phases.expirationEvents())
		channel.open()

		terminal, workflow := execution.handlePhaseExpiration(expiration)
		if terminal || workflow.cause != nil || !execution.attachStarted {
			t.Fatalf("receiver Establishing reconciliation failed: terminal=%t workflow=%+v attachStarted=%t",
				terminal, workflow, execution.attachStarted)
		}
		admission := receiveTest(t, timers.created)
		if admission.phase != PeerAttemptPhaseAdmission {
			t.Fatalf("next phase = %q, want %q", admission.phase, PeerAttemptPhaseAdmission)
		}
		receiveTest(t, lanes.requested)
		receiveTest(t, lanes.attachments)
		execution.children.Wait()
	})
}

func TestSenderStagedNegotiationTimeoutEmitsDeadlineDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name       string
		advance    func(*peerPhaseLifecycle, *recordingReceiverPhaseTimerSource) recordedReceiverPhaseTimer
		wantPhase  PeerAttemptPhase
		wantBudget time.Duration
	}{
		{
			name: "signaling preparation",
			advance: func(_ *peerPhaseLifecycle, timers *recordingReceiverPhaseTimerSource) recordedReceiverPhaseTimer {
				return receiveTest(t, timers.created)
			},
			wantPhase:  PeerAttemptPhasePreparation,
			wantBudget: PeerSignalingPreparationBudget,
		},
		{
			name: "ice checking",
			advance: func(phases *peerPhaseLifecycle, timers *recordingReceiverPhaseTimerSource) recordedReceiverPhaseTimer {
				receiveTest(t, timers.created)
				phases.observeICE(pion.ICEConnectionStateChecking)
				return receiveTest(t, timers.created)
			},
			wantPhase:  PeerAttemptPhaseChecking,
			wantBudget: PeerICECheckingBudget,
		},
		{
			name: "datachannel establishment",
			advance: func(phases *peerPhaseLifecycle, timers *recordingReceiverPhaseTimerSource) recordedReceiverPhaseTimer {
				receiveTest(t, timers.created)
				phases.observeICE(pion.ICEConnectionStateConnected)
				return receiveTest(t, timers.created)
			},
			wantPhase:  PeerAttemptPhaseEstablishing,
			wantBudget: PeerDataChannelEstablishmentBudget,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				timers := newRecordingReceiverPhaseTimerSource()
				collector := &senderObservationCollector{}
				factory := mustTestFactoryWithSenderCollector(t, collector, Config{PhaseTimers: timers})
				session := newTestPeerSession(146)
				binding := testBinding(147)
				attempt := newPeerAttempt(peerAttemptConfig{
					factory: factory, session: session,
					operation: testPeerOperation(testOperationID(148)),
					offer:     v2signalOffer(binding),
				})
				attempt.phases.staged = true
				attempt.recorder.begin()
				attempt.recorder.negotiationDeadlineArmed()
				attempt.recorder.complete(SenderAttemptAnswerCreated, SenderCandidateCounts{}, nil)
				attempt.recorder.complete(SenderAttemptAnswerSent, SenderCandidateCounts{}, nil)

				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				phaseContext, err := attempt.phases.beginNegotiation(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer attempt.phases.terminate(nil)

				stageTimer := tc.advance(attempt.phases, timers)
				if stageTimer.phase != tc.wantPhase || stageTimer.duration != tc.wantBudget {
					t.Fatalf("stage timer = %+v, want phase=%q budget=%s", stageTimer, tc.wantPhase, tc.wantBudget)
				}

				execution := newAttemptExecution(attempt, ctx, newTestPeerConnection())
				execution.phaseContext = phaseContext
				defer execution.candidateWriter.Close()

				stageTimer.timer.Fire()
				expiration := receiveTest(t, attempt.phases.expirationEvents())
				terminal, cause := execution.handlePhaseExpiration(expiration)
				if !terminal || !errors.Is(cause, ErrPeerNegotiationTimeout) {
					t.Fatalf("staged expiration = (%t, %v), want terminal ErrPeerNegotiationTimeout", terminal, cause)
				}
				_ = attempt.finish(ctx, cause, nil, false)
				wireFailure := receiveTest(t, session.failures)
				if wireFailure.code != protocolsession.PeerOperationCodeTimeout {
					t.Fatalf("wire failure = %+v, want timeout", wireFailure)
				}

				observations := collector.forAttempt(binding.AttemptID)
				expiredCount := 0
				var expiredEvent SenderAttemptObservation
				var failedEvent SenderAttemptObservation
				for _, event := range observations {
					if event.Stage == SenderAttemptNegotiationDeadlineExpired {
						expiredCount++
						expiredEvent = event
					}
					if event.Stage == SenderAttemptFailed {
						failedEvent = event
					}
				}
				if expiredCount != 1 || expiredEvent.Phase != SenderAttemptPhaseNegotiation ||
					expiredEvent.DeadlineMillis != durationMilliseconds(tc.wantBudget) {
					t.Fatalf("deadline expired observations count=%d event=%#v", expiredCount, expiredEvent)
				}
				if failedEvent.Failure == nil || !failedEvent.Failure.DeadlineExpired ||
					failedEvent.Failure.TypedPeerErrorCode != TypedPeerErrorTimeout ||
					failedEvent.Failure.Termination.Cause != "negotiation_timeout" {
					t.Fatalf("failed observation = %#v", failedEvent.Failure)
				}
			})
		})
	}
}

func TestSenderPreLoopNegotiationTimeoutEmitsDeadlineDiagnostics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		timers := newRecordingReceiverPhaseTimerSource()
		collector := &senderObservationCollector{}
		peer := newTestPeerConnection()
		entered := make(chan struct{})
		factory := mustTestFactoryWithSenderCollector(t, collector, Config{
			PhaseTimers: timers,
			PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
				return peer, nil
			}),
		})
		session := &blockingAnswerPeerSession{
			testPeerSession: newTestPeerSession(149),
			entered:         entered,
		}
		binding := testBinding(150)
		attempt := newPeerAttempt(peerAttemptConfig{
			factory: factory, session: session,
			operation: testPeerOperation(testOperationID(151)),
			offer:     v2signalOffer(binding),
			onDone:    func(*peerAttempt, error) {},
		})
		done := make(chan error, 1)
		go func() {
			done <- attempt.run(t.Context(), turnsession.OfferGrant{})
		}()

		negotiation := receiveTest(t, timers.created)
		receiveTest(t, entered)
		negotiation.timer.Fire()
		err := receiveTest(t, done)
		if !errors.Is(err, ErrPeerNegotiationTimeout) {
			t.Fatalf("pre-loop negotiate error = %v, want ErrPeerNegotiationTimeout", err)
		}

		observations := collector.forAttempt(binding.AttemptID)
		var sawExpired bool
		var failure *SenderAttemptFailure
		for _, event := range observations {
			if event.Stage == SenderAttemptNegotiationDeadlineExpired &&
				event.DeadlineMillis == durationMilliseconds(factory.negotiationBudget) {
				sawExpired = true
			}
			if event.Failure != nil {
				failure = event.Failure
			}
		}
		if !sawExpired || failure == nil || !failure.DeadlineExpired ||
			failure.Termination.Cause != "negotiation_timeout" {
			t.Fatalf("pre-loop diagnostics sawExpired=%t failure=%+v", sawExpired, failure)
		}
	})
}

type blockingAnswerPeerSession struct {
	*testPeerSession
	entered chan struct{}
}

func (session *blockingAnswerPeerSession) SendPeerControl(
	ctx context.Context,
	op protocolsession.Operation,
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	if kind == protocolsession.MessagePeerAnswer {
		close(session.entered)
		<-ctx.Done()
		return protocolsession.OperationDeliver, ctx.Err()
	}
	return session.testPeerSession.SendPeerControl(ctx, op, kind, body)
}

type uncanceledTestParent struct {
	done      <-chan struct{}
	doneCalls atomic.Int32
}

func (parent *uncanceledTestParent) Deadline() (time.Time, bool) { return time.Time{}, false }
func (parent *uncanceledTestParent) Done() <-chan struct{} {
	parent.doneCalls.Add(1)
	return parent.done
}
func (parent *uncanceledTestParent) Err() error    { return nil }
func (parent *uncanceledTestParent) Value(any) any { return nil }

type failingStageTimerSource struct {
	inner     *recordingReceiverPhaseTimerSource
	failPhase PeerAttemptPhase
	err       error
}

func (source *failingStageTimerSource) NewPeerPhaseTimer(
	phase PeerAttemptPhase,
	duration time.Duration,
) (PeerPhaseTimer, error) {
	if phase == source.failPhase {
		return nil, source.err
	}
	return source.inner.NewPeerPhaseTimer(phase, duration)
}

func TestNativeObservedStagesDoNotLeakIntermediateContexts(t *testing.T) {
	t.Run("terminate after establishing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			parent := &uncanceledTestParent{done: make(chan struct{})}
			timers := newRecordingReceiverPhaseTimerSource()
			lifecycle := newPeerPhaseLifecycle(timers, DefaultPeerNegotiationBudget, DefaultPeerAdmissionBudget)
			lifecycle.staged = true

			ctx, err := lifecycle.beginNegotiation(parent)
			if err != nil {
				t.Fatal(err)
			}
			preparation := receiveTest(t, timers.created)
			synctest.Wait()
			doneCallsAfterBegin := parent.doneCalls.Load()

			lifecycle.observeICE(pion.ICEConnectionStateChecking)
			checking := receiveTest(t, timers.created)
			lifecycle.observeICE(pion.ICEConnectionStateConnected)
			establishing := receiveTest(t, timers.created)
			synctest.Wait()

			if !preparation.timer.stopped.Load() || !checking.timer.stopped.Load() || establishing.timer.stopped.Load() {
				t.Fatalf("stage timer stop states = (%t, %t, %t), want (true, true, false)",
					preparation.timer.stopped.Load(), checking.timer.stopped.Load(), establishing.timer.stopped.Load())
			}
			if ctx.Err() != nil {
				t.Fatalf("stage transition canceled negotiation context: %v", context.Cause(ctx))
			}
			if got := parent.doneCalls.Load(); got != doneCallsAfterBegin {
				t.Fatalf("observeICE attached intermediate child contexts to parent: Done() calls %d -> %d",
					doneCallsAfterBegin, got)
			}

			lifecycle.terminate(context.Canceled)
			if !errors.Is(context.Cause(ctx), context.Canceled) || !establishing.timer.stopped.Load() {
				t.Fatalf("terminate did not release negotiation deadline: cause=%v stopped=%t",
					context.Cause(ctx), establishing.timer.stopped.Load())
			}
		})
	})

	t.Run("admission after establishing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			parent := &uncanceledTestParent{done: make(chan struct{})}
			timers := newRecordingReceiverPhaseTimerSource()
			lifecycle := newPeerPhaseLifecycle(timers, DefaultPeerNegotiationBudget, DefaultPeerAdmissionBudget)
			lifecycle.staged = true

			ctx, err := lifecycle.beginNegotiation(parent)
			if err != nil {
				t.Fatal(err)
			}
			receiveTest(t, timers.created)
			lifecycle.observeICE(pion.ICEConnectionStateChecking)
			receiveTest(t, timers.created)
			lifecycle.observeICE(pion.ICEConnectionStateConnected)
			establishing := receiveTest(t, timers.created)

			admissionCtx, started, err := lifecycle.beginAdmission(parent)
			if err != nil || !started {
				t.Fatalf("beginAdmission = (%v, %t, %v)", admissionCtx, started, err)
			}
			if !errors.Is(context.Cause(ctx), context.Canceled) || !establishing.timer.stopped.Load() {
				t.Fatalf("beginAdmission left negotiation active: cause=%v stopped=%t",
					context.Cause(ctx), establishing.timer.stopped.Load())
			}
			admission := receiveTest(t, timers.created)
			lifecycle.terminate(context.Canceled)
			if !errors.Is(context.Cause(admissionCtx), context.Canceled) || !admission.timer.stopped.Load() {
				t.Fatalf("terminate left admission active: cause=%v stopped=%t",
					context.Cause(admissionCtx), admission.timer.stopped.Load())
			}
		})
	})

	t.Run("timer error cancels negotiation without leak", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			parent := &uncanceledTestParent{done: make(chan struct{})}
			boom := errors.New("stage timer unavailable")
			inner := newRecordingReceiverPhaseTimerSource()
			timers := &failingStageTimerSource{
				inner:     inner,
				failPhase: PeerAttemptPhaseEstablishing,
				err:       boom,
			}
			lifecycle := newPeerPhaseLifecycle(timers, DefaultPeerNegotiationBudget, DefaultPeerAdmissionBudget)
			lifecycle.staged = true

			ctx, err := lifecycle.beginNegotiation(parent)
			if err != nil {
				t.Fatal(err)
			}
			receiveTest(t, inner.created)
			lifecycle.observeICE(pion.ICEConnectionStateChecking)
			checking := receiveTest(t, inner.created)

			lifecycle.observeICE(pion.ICEConnectionStateConnected)
			if !checking.timer.stopped.Load() {
				t.Fatal("failed stage transition did not stop previous timer")
			}
			if !errors.Is(context.Cause(ctx), ErrConfig) || !errors.Is(context.Cause(ctx), boom) {
				t.Fatalf("failed stage transition cause = %v, want ErrConfig + %v", context.Cause(ctx), boom)
			}
			if _, started, err := lifecycle.beginAdmission(parent); started || err != nil {
				t.Fatalf("terminal lifecycle admitted after timer failure: started=%t err=%v", started, err)
			}
		})
	})
}

type syncPhaseTimer struct {
	fire chan time.Time
}

func (timer *syncPhaseTimer) C() <-chan time.Time { return timer.fire }
func (timer *syncPhaseTimer) Stop()               {}

type recordedSyncPhaseTimer struct {
	phase PeerAttemptPhase
	fire  chan time.Time
}

type syncPhaseTimerSource struct {
	created chan recordedSyncPhaseTimer
}

func newSyncPhaseTimerSource() *syncPhaseTimerSource {
	return &syncPhaseTimerSource{created: make(chan recordedSyncPhaseTimer, 8)}
}

func (source *syncPhaseTimerSource) NewPeerPhaseTimer(
	phase PeerAttemptPhase,
	_ time.Duration,
) (PeerPhaseTimer, error) {
	timer := &syncPhaseTimer{fire: make(chan time.Time)}
	source.created <- recordedSyncPhaseTimer{phase: phase, fire: timer.fire}
	return timer, nil
}

func TestNativeObservedStageAdvanceIgnoresRacingRetiredTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		timers := newSyncPhaseTimerSource()
		lifecycle := newPeerPhaseLifecycle(timers, DefaultPeerNegotiationBudget, DefaultPeerAdmissionBudget)
		lifecycle.staged = true

		ctx, err := lifecycle.beginNegotiation(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer lifecycle.terminate(context.Canceled)
		preparation := receiveTest(t, timers.created)

		// Synchronize on the unbuffered timer channel while holding lifecycle.mu
		// so the retired timer goroutine has already won select <-timer.C() but
		// observes the superseded generation inside deadlineFired.
		lifecycle.mu.Lock()
		preparation.fire <- time.Now()
		lifecycle.observeICELocked(pion.ICEConnectionStateChecking)
		lifecycle.mu.Unlock()
		checking := receiveTest(t, timers.created)
		synctest.Wait()

		if ctx.Err() != nil {
			t.Fatalf("racing Preparation timer canceled Checking stage: %v", context.Cause(ctx))
		}
		select {
		case exp := <-lifecycle.expirationEvents():
			t.Fatalf("racing Preparation timer emitted stale expiration: %+v", exp)
		default:
		}

		lifecycle.mu.Lock()
		checking.fire <- time.Now()
		lifecycle.observeICELocked(pion.ICEConnectionStateConnected)
		lifecycle.mu.Unlock()
		establishing := receiveTest(t, timers.created)
		synctest.Wait()

		if ctx.Err() != nil {
			t.Fatalf("racing Checking timer canceled Establishing stage: %v", context.Cause(ctx))
		}
		select {
		case exp := <-lifecycle.expirationEvents():
			t.Fatalf("racing Checking timer emitted stale expiration: %+v", exp)
		default:
		}

		establishing.fire <- time.Now()
		receiveTest(t, ctx.Done())
		if !errors.Is(context.Cause(ctx), ErrPeerNegotiationTimeout) {
			t.Fatalf("Establishing timeout cause = %v, want ErrPeerNegotiationTimeout", context.Cause(ctx))
		}
		if exp := receiveTest(t, lifecycle.expirationEvents()); exp.phase != PeerAttemptPhaseEstablishing {
			t.Fatalf("expiration phase = %q, want %q", exp.phase, PeerAttemptPhaseEstablishing)
		}
	})
}
