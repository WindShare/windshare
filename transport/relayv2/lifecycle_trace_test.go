package relayv2

import (
	"context"
	"errors"
	"sync"
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
	link := newLinkWithLifecycleStream(
		context.Background(), newScriptedSocket(), false, DefaultLifecycleObservationCapacity,
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
	link.resolveRequest(request, nil, true)
	if err := waitRelayResult(t, result); err != nil {
		t.Fatalf("ordinary send: %v", err)
	}
	select {
	case event := <-link.lifecycleTrace():
		t.Fatalf("ordinary success produced lifecycle work: %+v", event)
	default:
	}
	link.stop(nil)
}

func TestRelayLifecycleFailureAndRefusalRemainObservable(t *testing.T) {
	t.Run("accepted provider failure", func(t *testing.T) {
		link := newLinkWithLifecycleStream(
			context.Background(), newScriptedSocket(), false, DefaultLifecycleObservationCapacity,
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
		link.resolveRequest(request, providerFailure, true)
		if err := waitRelayResult(t, result); !errors.Is(err, providerFailure) ||
			framechannel.SendDispositionOf(err) != framechannel.SendAccepted {
			t.Fatalf("accepted provider failure = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		event := waitLifecycleTrace(t, link.lifecycleTrace(), LifecycleSendAdmitted)
		if event.RelaySessionID != channel.id || event.OperationID != request.operationID || event.Terminal ||
			event.Disposition != framechannel.SendAccepted || event.RetirementSource != LifecycleRetirementNone ||
			event.Cause != LifecycleCauseTransport || event.DrainCause != LifecycleCauseNone {
			t.Fatalf("accepted provider failure trace = %+v", event)
		}
		link.stop(nil)
	})

	t.Run("rejection", func(t *testing.T) {
		link := newLinkWithLifecycleStream(
			context.Background(), newScriptedSocket(), false, DefaultLifecycleObservationCapacity,
		)
		channel := link.installFixed(relaySessionID(97))
		err := channel.Send(context.Background(), nil)
		if !errors.Is(err, ErrFrameBounds) || framechannel.SendDispositionOf(err) != framechannel.SendRejected {
			t.Fatalf("invalid send = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		event := waitLifecycleTrace(t, link.lifecycleTrace(), LifecycleSendRejected)
		if event.RetirementSource != LifecycleRetirementNone || event.Cause != LifecycleCauseFrameBounds ||
			event.DrainCause != LifecycleCauseNone {
			t.Fatalf("rejection trace = %+v", event)
		}
		link.stop(nil)
	})

	t.Run("rollback", func(t *testing.T) {
		link := newLinkWithLifecycleStream(
			context.Background(), newScriptedSocket(), false, DefaultLifecycleObservationCapacity,
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
		event := waitLifecycleTrace(t, link.lifecycleTrace(), LifecycleSendRolledBack)
		if event.RetirementSource != LifecycleRetirementNone || event.Cause != LifecycleCauseCanceled ||
			event.DrainCause != LifecycleCauseNone {
			t.Fatalf("rollback trace = %+v", event)
		}
		link.stop(nil)
	})

	t.Run("terminal provider failure", func(t *testing.T) {
		link := newLinkWithLifecycleStream(
			context.Background(), newScriptedSocket(), false, DefaultLifecycleObservationCapacity,
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
		link.resolveRequest(request, providerFailure, true)
		if err := waitRelayResult(t, result); !errors.Is(err, providerFailure) ||
			framechannel.SendDispositionOf(err) != framechannel.SendAccepted {
			t.Fatalf("terminal provider failure = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		event := waitLifecycleTrace(t, link.lifecycleTrace(), LifecycleTerminalSettled)
		if !event.Terminal || event.Disposition != framechannel.SendAccepted ||
			event.RetirementSource != LifecycleRetirementNone || event.Cause != LifecycleCauseTransport ||
			event.DrainCause != LifecycleCauseNone {
			t.Fatalf("terminal settlement trace = %+v", event)
		}
		link.stop(nil)
	})
}

func TestRelayLifecycleDisabledHasNoProducerWork(t *testing.T) {
	link := newLink(context.Background(), newScriptedSocket(), false)
	if link.traces != nil || link.lifecycleTrace() != nil {
		t.Fatal("disabled lifecycle observations allocated a producer")
	}
	link.stop(nil)
	if completion := link.completeObservations(); completion != (LifecycleObservationCompletion{}) {
		t.Fatalf("disabled completion = %+v", completion)
	}
}

func TestRelayLifecycleNoReaderSaturationCannotDelayShutdown(t *testing.T) {
	link := newLinkWithLifecycleStream(context.Background(), newScriptedSocket(), false, 1)
	channel := link.installFixed(relaySessionID(93))
	if err := channel.Send(context.Background(), nil); !errors.Is(err, ErrFrameBounds) {
		t.Fatalf("invalid send = %v", err)
	}

	stopped := make(chan struct{})
	go func() {
		link.stop(nil)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("full lifecycle stream delayed relay shutdown")
	}

	completion := link.completeObservations()
	if completion.Enqueued != 1 || completion.Loss.CapacityDropped == 0 {
		t.Fatalf("saturated completion = %+v", completion)
	}
	var events []LifecycleTrace
	for event := range link.lifecycleTrace() {
		events = append(events, event)
	}
	if len(events) != 1 || events[0].Stage != LifecycleSendRejected {
		t.Fatalf("retained saturated prefix = %+v", events)
	}
}

func TestRelayLifecyclePublishesLinkScopedDropSummaryWhenSpaceReturns(t *testing.T) {
	source := newLifecycleSource(17, 2)
	sessionID := relaySessionID(94)
	source.emit(sendRejectedTrace(sessionID, 1, false, framechannel.SendRejected, LifecycleCauseCanceled))
	source.emit(sendRejectedTrace(sessionID, 2, false, framechannel.SendRejected, LifecycleCauseCanceled))
	if source.emit(sendRejectedTrace(sessionID, 3, false, framechannel.SendRejected, LifecycleCauseCanceled)) {
		t.Fatal("saturated lifecycle event was retained")
	}

	<-source.stream()
	<-source.stream()
	if !source.emit(sendRejectedTrace(sessionID, 4, false, framechannel.SendRejected, LifecycleCauseCanceled)) {
		t.Fatal("lifecycle source did not resume after consumer progress")
	}
	summary := <-source.stream()
	if summary.LinkID != 17 || summary.Stage != LifecycleTraceDropped || summary.Dropped != 1 {
		t.Fatalf("drop summary = %+v", summary)
	}
	assertRelayLifecycleSourceShape(t, summary)
	if event := <-source.stream(); event.OperationID != 4 {
		t.Fatalf("post-gap event = %+v", event)
	}

	completion := source.complete()
	if completion.Enqueued != 4 || completion.Loss.CapacityDropped != 1 ||
		completion.Loss.Total() != 1 {
		t.Fatalf("summary completion = %+v", completion)
	}
}

func TestRelayLifecycleCompletionIsImmediateIdempotentAndAuthoritative(t *testing.T) {
	source := newLifecycleSource(18, 1)
	sessionID := relaySessionID(95)
	source.emit(sendRejectedTrace(sessionID, 1, false, framechannel.SendRejected, LifecycleCauseCanceled))
	source.emit(sendRejectedTrace(sessionID, 2, false, framechannel.SendRejected, LifecycleCauseCanceled))

	first := source.complete()
	second := source.complete()
	if first != second {
		t.Fatalf("completion changed: first=%+v second=%+v", first, second)
	}
	if first.Enqueued != 1 || first.Loss.CapacityDropped != 1 {
		t.Fatalf("completion = %+v", first)
	}
	if source.emit(sendRejectedTrace(sessionID, 3, false, framechannel.SendRejected, LifecycleCauseCanceled)) {
		t.Fatal("publication succeeded after completion")
	}
	if event, open := <-source.stream(); !open || event.OperationID != 1 {
		t.Fatalf("retained event after completion open=%t event=%+v", open, event)
	}
	if _, open := <-source.stream(); open {
		t.Fatal("lifecycle stream remained open after retained prefix")
	}
}

func TestRelayLifecycleDropSummaryPrecedesRetainedFinalFact(t *testing.T) {
	source := newLifecycleSource(19, 2)
	sessionID := relaySessionID(103)
	for operationID := uint64(1); operationID <= 2; operationID++ {
		source.emit(sendRejectedTrace(
			sessionID, operationID, false, framechannel.SendRejected, LifecycleCauseCanceled,
		))
	}
	source.emit(sendRejectedTrace(
		sessionID, 3, false, framechannel.SendRejected, LifecycleCauseCanceled,
	))
	<-source.stream()
	<-source.stream()

	completion := source.completeWithFinal(linkClosedTrace(
		4, LifecycleRetirementLinkFailure, LifecycleCauseTransport,
	))
	var events []LifecycleTrace
	for event := range source.stream() {
		events = append(events, event)
	}
	if len(events) != 2 || events[0].Stage != LifecycleTraceDropped ||
		events[0].Dropped != 1 || events[1].Stage != LifecycleLinkClosed {
		t.Fatalf("final gap ordering = %+v", events)
	}
	if completion.Enqueued != 4 || completion.Loss.CapacityDropped != 1 {
		t.Fatalf("final completion = %+v", completion)
	}
}

func TestRelayLifecycleFailurePublishesAdmittedSendResultsBeforeCompletion(t *testing.T) {
	for _, test := range []struct {
		name     string
		terminal bool
		stage    LifecycleStage
	}{
		{name: "ordinary failure", stage: LifecycleSendAdmitted},
		{name: "terminal settlement", terminal: true, stage: LifecycleTerminalSettled},
	} {
		t.Run(test.name, func(t *testing.T) {
			link := newLinkWithLifecycleStream(
				context.Background(), newScriptedSocket(), false, DefaultLifecycleObservationCapacity,
			)
			channel := link.installFixed(relaySessionID(104))
			result := make(chan error, 1)
			go func() {
				if test.terminal {
					result <- channel.SendTerminal(context.Background(), framechannel.Frame("owned"))
					return
				}
				result <- channel.Send(context.Background(), framechannel.Frame("owned"))
			}()
			requireQueuedRequests(t, link, channel.id, 1)
			request, ok := link.takeRequest()
			if !ok {
				t.Fatal("accepted send did not reach write ownership")
			}

			linkFailure := errors.New("failure while accepted write is unresolved")
			stopped := make(chan struct{})
			go func() {
				link.stop(linkFailure)
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("link did not resolve the admitted send during failure shutdown")
			}
			if err := waitRelayResult(t, result); !errors.Is(err, linkFailure) {
				t.Fatalf("accepted send result = %v", err)
			}

			var events []LifecycleTrace
			for event := range link.lifecycleTrace() {
				events = append(events, event)
			}
			acceptedIndex, closedIndex := -1, -1
			for index, event := range events {
				switch event.Stage {
				case test.stage:
					if event.OperationID == request.operationID && event.Cause == LifecycleCauseClosed {
						acceptedIndex = index
					}
				case LifecycleLinkClosed:
					closedIndex = index
				}
			}
			if acceptedIndex < 0 || closedIndex < 0 || acceptedIndex >= closedIndex {
				t.Fatalf("accepted-result/link-close ordering = %+v", events)
			}
			completion := link.completeObservations()
			if completion.Enqueued != uint64(len(events)) || completion.Loss.Total() != 0 {
				t.Fatalf("completion = %+v events=%d", completion, len(events))
			}
		})
	}
}

func TestRelayLifecycleShutdownOrdersTerminalFactBeforeStreamCloseAndDone(t *testing.T) {
	const concurrentEmits = 32
	link := newLinkWithLifecycleStream(context.Background(), newScriptedSocket(), false, 128)
	channel := link.installFixed(relaySessionID(102))

	var publishers sync.WaitGroup
	publishers.Add(concurrentEmits)
	for range concurrentEmits {
		go func() {
			defer publishers.Done()
			_ = channel.Send(context.Background(), nil)
		}()
	}
	stopDone := make(chan struct{})
	go func() {
		link.stop(nil)
		close(stopDone)
	}()
	publishers.Wait()
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent lifecycle publication delayed stop")
	}

	var events []LifecycleTrace
	for event := range link.lifecycleTrace() {
		events = append(events, event)
	}
	if len(events) == 0 || events[len(events)-1].Stage != LifecycleLinkClosed {
		t.Fatalf("terminal lifecycle ordering = %+v", events)
	}
	first := link.completeObservations()
	var completions [8]LifecycleObservationCompletion
	var completionsWait sync.WaitGroup
	completionsWait.Add(len(completions))
	for index := range completions {
		go func() {
			defer completionsWait.Done()
			completions[index] = link.completeObservations()
		}()
	}
	completionsWait.Wait()
	for index, completion := range completions {
		if completion != first {
			t.Fatalf("completion[%d]=%+v want %+v", index, completion, first)
		}
	}
	select {
	case <-link.done:
	default:
		t.Fatal("link done was not closed after lifecycle stream completion")
	}
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
