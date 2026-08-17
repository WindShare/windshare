package relayv2

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/framechannel"
)

func TestRelayLifecycleConstructorsProduceStageShapes(t *testing.T) {
	sessionID := relaySessionID(91)
	transportCause := LifecycleCauseTransport
	tests := []struct {
		name  string
		trace LifecycleTrace
	}{
		{"terminal reserved", terminalReservedTrace(sessionID, 1)},
		{"terminal admitted", terminalSendAdmittedTrace(sessionID, 2)},
		{"accepted provider failure", acceptedSendFailureTrace(sessionID, 3, transportCause)},
		{"send rejected", sendRejectedTrace(sessionID, 4, false, framechannel.SendRejected, LifecycleCauseCanceled)},
		{"send rolled back", sendRolledBackTrace(sessionID, 5, true, LifecycleCauseDeadline)},
		{"retirement deferred", retirementDeferredTrace(sessionID, 6, true, LifecycleRetirementLocalClose, LifecycleCauseNone, LifecycleCauseNone)},
		{"retired", retiredTrace(sessionID, 7, false, LifecycleRetirementIngressFailure, LifecycleCauseIngressOverflow, LifecycleCauseIngressOverflow)},
		{"terminal settled", terminalSettledTrace(sessionID, 8, LifecycleCauseNone)},
		{"link retiring", linkRetiringTrace(9, LifecycleRetirementLinkFailure, transportCause, transportCause)},
		{"link closed", linkClosedTrace(10, LifecycleRetirementLinkClose, LifecycleCauseNone)},
		{"drop summary", traceDroppedSummary(11, 12)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertRelayLifecycleSourceShape(t, test.trace)
		})
	}
}

func TestRelayLifecycleOrdinarySuccessIsSuppressed(t *testing.T) {
	var observed atomic.Uint64
	link := newLinkWithTracer(
		context.Background(),
		newScriptedSocket(),
		false,
		LifecycleTraceFunc(func(LifecycleTrace) { observed.Add(1) }),
	)
	channel := link.installFixed(relaySessionID(92))

	result := make(chan error, 1)
	go func() {
		result <- channel.Send(context.Background(), framechannel.Frame("ordinary"))
	}()
	requireQueuedRequests(t, link, channel.id, 1)
	request, ok := link.takeRequest()
	if !ok {
		t.Fatal("ordinary send did not reach provider ownership")
	}
	request.receipt <- nil
	if err := waitRelayResult(t, result); err != nil {
		t.Fatalf("ordinary send: %v", err)
	}

	link.traces.mu.Lock()
	queued, dropped := len(link.traces.queue), link.traces.loss.Total()
	link.traces.mu.Unlock()
	if observed.Load() != 0 || queued != 0 || dropped != 0 {
		t.Fatalf("ordinary success produced lifecycle work: observed=%d queued=%d dropped=%d", observed.Load(), queued, dropped)
	}
	link.stop(nil)
}

func TestRelayLifecycleAcceptedProviderFailureRemainsObservable(t *testing.T) {
	events := make(chan LifecycleTrace, 4)
	link := newLinkWithTracer(
		context.Background(),
		newScriptedSocket(),
		false,
		LifecycleTraceFunc(func(event LifecycleTrace) { events <- event }),
	)
	channel := link.installFixed(relaySessionID(96))
	result := make(chan error, 1)
	providerFailure := errors.New("provider failed after acceptance")
	go func() {
		result <- channel.Send(context.Background(), framechannel.Frame("ordinary"))
	}()
	requireQueuedRequests(t, link, channel.id, 1)
	request, ok := link.takeRequest()
	if !ok {
		t.Fatal("ordinary send did not reach provider ownership")
	}
	request.receipt <- providerFailure
	if err := waitRelayResult(t, result); !errors.Is(err, providerFailure) ||
		framechannel.SendDispositionOf(err) != framechannel.SendAccepted {
		t.Fatalf("accepted provider failure = %v disposition=%d", err, framechannel.SendDispositionOf(err))
	}
	event := waitLifecycleTrace(t, events, LifecycleSendAdmitted)
	if event.RelaySessionID != channel.id || event.OperationID != request.operationID || event.Terminal ||
		event.Disposition != framechannel.SendAccepted || event.RetirementSource != LifecycleRetirementNone ||
		event.Cause != LifecycleCauseTransport || event.DrainCause != LifecycleCauseNone {
		t.Fatalf("accepted provider failure trace = %+v", event)
	}
	link.stop(nil)
}

func TestRelayLifecycleRefusalRollbackAndTerminalFailureRemainObservable(t *testing.T) {
	t.Run("rejection", func(t *testing.T) {
		events := make(chan LifecycleTrace, 4)
		link := newLinkWithTracer(
			context.Background(), newScriptedSocket(), false,
			LifecycleTraceFunc(func(event LifecycleTrace) { events <- event }),
		)
		channel := link.installFixed(relaySessionID(97))
		err := channel.Send(context.Background(), nil)
		if !errors.Is(err, ErrFrameBounds) || framechannel.SendDispositionOf(err) != framechannel.SendRejected {
			t.Fatalf("invalid send = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		event := waitLifecycleTrace(t, events, LifecycleSendRejected)
		if event.RetirementSource != LifecycleRetirementNone || event.Cause != LifecycleCauseFrameBounds ||
			event.DrainCause != LifecycleCauseNone {
			t.Fatalf("rejection trace = %+v", event)
		}
		link.stop(nil)
	})

	t.Run("rollback", func(t *testing.T) {
		events := make(chan LifecycleTrace, 4)
		link := newLinkWithTracer(
			context.Background(), newScriptedSocket(), false,
			LifecycleTraceFunc(func(event LifecycleTrace) { events <- event }),
		)
		channel := link.installFixed(relaySessionID(98))
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- channel.Send(ctx, framechannel.Frame("queued")) }()
		requireQueuedRequests(t, link, channel.id, 1)
		cancel()
		if err := waitRelayResult(t, result); !errors.Is(err, context.Canceled) ||
			framechannel.SendDispositionOf(err) != framechannel.SendRejected {
			t.Fatalf("rolled-back send = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		event := waitLifecycleTrace(t, events, LifecycleSendRolledBack)
		if event.RetirementSource != LifecycleRetirementNone || event.Cause != LifecycleCauseCanceled ||
			event.DrainCause != LifecycleCauseNone {
			t.Fatalf("rollback trace = %+v", event)
		}
		link.stop(nil)
	})

	t.Run("terminal provider failure", func(t *testing.T) {
		events := make(chan LifecycleTrace, 8)
		link := newLinkWithTracer(
			context.Background(), newScriptedSocket(), false,
			LifecycleTraceFunc(func(event LifecycleTrace) { events <- event }),
		)
		channel := link.installFixed(relaySessionID(99))
		result := make(chan error, 1)
		providerFailure := errors.New("terminal provider failure")
		go func() {
			result <- channel.SendTerminal(context.Background(), framechannel.Frame("terminal"))
		}()
		requireQueuedRequests(t, link, channel.id, 1)
		request, ok := link.takeRequest()
		if !ok {
			t.Fatal("terminal send did not reach provider ownership")
		}
		request.receipt <- providerFailure
		if err := waitRelayResult(t, result); !errors.Is(err, providerFailure) ||
			framechannel.SendDispositionOf(err) != framechannel.SendAccepted {
			t.Fatalf("terminal provider failure = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		event := waitLifecycleTrace(t, events, LifecycleTerminalSettled)
		if !event.Terminal || event.Disposition != framechannel.SendAccepted ||
			event.RetirementSource != LifecycleRetirementNone || event.Cause != LifecycleCauseTransport ||
			event.DrainCause != LifecycleCauseNone {
			t.Fatalf("terminal settlement trace = %+v", event)
		}
		link.stop(nil)
	})
}

func TestRelayLifecycleNilTracerHasNoDispatcher(t *testing.T) {
	link := newLinkWithTracer(context.Background(), newScriptedSocket(), false, nil)
	if link.traces != nil {
		t.Fatal("nil tracer allocated a relay lifecycle dispatcher")
	}
	link.stop(nil)
}

func TestRelayLifecycleBlockedObserverCannotDelayTransport(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var first sync.Once
	link := newLinkWithTracer(
		context.Background(),
		newScriptedSocket(),
		false,
		LifecycleTraceFunc(func(LifecycleTrace) {
			first.Do(func() {
				close(entered)
				<-release
			})
		}),
	)
	channel := link.installFixed(relaySessionID(93))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	sendResult := make(chan error, 1)
	go func() { sendResult <- channel.Send(canceled, framechannel.Frame("rejected")) }()
	if err := waitRelayResult(t, sendResult); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled send = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("relay observer did not enter blocking callback")
	}

	closeResult := make(chan error, 1)
	go func() { closeResult <- channel.Close() }()
	if err := waitRelayResult(t, closeResult); err != nil {
		t.Fatalf("channel close: %v", err)
	}
	stopped := make(chan struct{})
	go func() {
		link.stop(nil)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("blocked observer delayed relay link close")
	}
}

func TestRelayLifecycleDispatcherCoalescesOverflowAndPanic(t *testing.T) {
	t.Run("overflow", func(t *testing.T) {
		const overflow = uint64(5)
		entered := make(chan struct{})
		release := make(chan struct{})
		observed := make(chan LifecycleTrace, relayLifecycleQueueCapacity+2)
		var first sync.Once
		dispatcher := newLifecycleDispatcher(17, LifecycleTraceFunc(func(event LifecycleTrace) {
			first.Do(func() {
				close(entered)
				<-release
			})
			observed <- event
		}))
		dispatcher.emit(terminalReservedTrace(relaySessionID(94), 1))
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("relay observer did not enter overflow gate")
		}
		for operationID := uint64(1); operationID <= relayLifecycleQueueCapacity+overflow; operationID++ {
			dispatcher.emit(sendRejectedTrace(
				relaySessionID(94), operationID, false,
				framechannel.SendRejected, LifecycleCauseCanceled,
			))
		}
		dispatcher.shutdown()
		close(release)
		for {
			select {
			case event := <-observed:
				if event.Stage == LifecycleTraceDropped {
					if event.LinkID != 17 || event.Dropped != overflow {
						t.Fatalf("drop summary = %+v, want link=17 dropped=%d", event, overflow)
					}
					assertRelayLifecycleSourceShape(t, event)
					completion := dispatcher.complete(context.Background())
					if !completion.Drained || completion.Loss.QueueOverflow != overflow ||
						completion.Loss.Total() != overflow {
						t.Fatalf("overflow completion = %+v", completion)
					}
					return
				}
			case <-time.After(time.Second):
				t.Fatal("relay overflow did not publish its coalesced drop summary")
			}
		}
	})

	t.Run("observer panic", func(t *testing.T) {
		var calls atomic.Uint64
		dispatcher := newLifecycleDispatcher(18, LifecycleTraceFunc(func(LifecycleTrace) {
			calls.Add(1)
			panic("observer panic must remain outside transport authority")
		}))
		dispatcher.emit(terminalReservedTrace(relaySessionID(95), 1))
		dispatcher.shutdown()
		completion := dispatcher.complete(context.Background())
		if !completion.Drained || completion.Delivered != 0 || calls.Load() != 1 ||
			completion.Loss.ObserverPanic != 1 || completion.Loss.Total() != 1 {
			t.Fatalf("panic completion = %+v", completion)
		}
	})

	t.Run("saturation", func(t *testing.T) {
		dispatcher := &lifecycleDispatcher{
			linkID: 19,
			loss:   LifecycleObservationLoss{QueueOverflow: ^uint64(0)},
			queue:  make([]LifecycleTrace, 0, relayLifecycleQueueCapacity),
		}
		dispatcher.flushDropSummaryLocked()
		if len(dispatcher.queue) != 1 || dispatcher.queue[0].Dropped != ^uint64(0) {
			t.Fatalf("saturated drop summary = %+v", dispatcher.queue)
		}
	})
}

func TestRelayLifecycleValidatorOwnsRetirementConsequenceShape(t *testing.T) {
	event := retiredTrace(
		relaySessionID(101), 7, false, LifecycleRetirementIngressFailure,
		LifecycleCauseIngressOverflow, LifecycleCauseTransport,
	)
	event.LinkID = 44
	if violation := ValidateLifecycleTrace(event); violation != LifecycleContractValid {
		t.Fatalf("non-none retirement drain violation=%d event=%+v", violation, event)
	}

	tests := []struct {
		name string
		edit func(*LifecycleTrace)
		want LifecycleContractViolation
	}{
		{"unknown stage", func(value *LifecycleTrace) { value.Stage = "future" }, LifecycleContractUnknownEnum},
		{"missing link", func(value *LifecycleTrace) { value.LinkID = 0 }, LifecycleContractInvalidIdentity},
		{"missing retirement source", func(value *LifecycleTrace) { value.RetirementSource = LifecycleRetirementNone }, LifecycleContractInvalidStageFields},
		{"unexpected disposition", func(value *LifecycleTrace) { value.Disposition = framechannel.SendAccepted }, LifecycleContractInvalidStageFields},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := event
			test.edit(&mutated)
			if got := ValidateLifecycleTrace(mutated); got != test.want {
				t.Fatalf("violation=%d want=%d event=%+v", got, test.want, mutated)
			}
		})
	}
}

func TestRelayLifecycleCompletionAccountsTimeoutAndStopsCallbackAdmission(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	var calls atomic.Uint64
	var committed atomic.Uint64
	dispatcher := newLifecycleDispatcher(45, LifecycleContextTraceFunc(func(ctx context.Context, _ LifecycleTrace) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			if ctx.Err() == nil {
				committed.Add(1)
			}
			close(exited)
		}
	}))
	dispatcher.callbackLimit = 10 * time.Millisecond
	for operationID := uint64(1); operationID <= 4; operationID++ {
		dispatcher.emit(sendRejectedTrace(
			relaySessionID(102), operationID, false,
			framechannel.SendRejected, LifecycleCauseCanceled,
		))
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("observer callback did not begin")
	}
	completion := dispatcher.complete(context.Background())
	if completion.Drained || completion.Delivered != 0 ||
		completion.Loss.CallbackTimeout != 1 || completion.Loss.Undrained != 3 {
		t.Fatalf("completion = %+v", completion)
	}
	dispatcher.emit(sendRejectedTrace(
		relaySessionID(102), 5, false, framechannel.SendRejected, LifecycleCauseCanceled,
	))
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("revoked relay callback did not exit")
	}
	if calls.Load() != 1 {
		t.Fatalf("callbacks begun after completion = %d", calls.Load())
	}
	if committed.Load() != 0 {
		t.Fatalf("revoked callback committed %d late fact(s)", committed.Load())
	}
}

func assertRelayLifecycleSourceShape(t *testing.T, event LifecycleTrace) {
	t.Helper()
	if event.LinkID == 0 {
		event.LinkID = 1
	}
	if violation := ValidateLifecycleTrace(event); violation != LifecycleContractValid {
		t.Fatalf("lifecycle source violation=%d event=%+v", violation, event)
	}
}

func waitRelayResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("relay operation did not complete")
		return nil
	}
}
