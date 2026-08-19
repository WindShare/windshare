package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestSenderTerminalPairsAreClosed(t *testing.T) {
	pairs := []struct {
		trigger    SenderSessionTerminalTrigger
		provenance SenderSessionTerminalProvenance
	}{
		{SenderSessionTerminalTriggerGracefulStop, SenderSessionTerminalProvenanceNormalStop},
		{SenderSessionTerminalTriggerForcedClose, SenderSessionTerminalProvenanceCallerStop},
		{SenderSessionTerminalTriggerPeerTerminal, SenderSessionTerminalProvenanceRemoteClose},
		{SenderSessionTerminalTriggerPathsExhausted, SenderSessionTerminalProvenanceLaneRetirement},
		{SenderSessionTerminalTriggerRuntimeFailed, SenderSessionTerminalProvenanceLocalFault},
	}
	for triggerIndex, trigger := range pairs {
		for provenanceIndex, provenance := range pairs {
			valid := validSenderSessionTerminalPair(trigger.trigger, provenance.provenance)
			if valid != (triggerIndex == provenanceIndex) {
				t.Fatalf(
					"pair %q/%q validity=%t, want %t",
					trigger.trigger,
					provenance.provenance,
					valid,
					triggerIndex == provenanceIndex,
				)
			}
		}
	}
	if validSenderSessionTerminalPair("unknown", SenderSessionTerminalProvenanceNormalStop) {
		t.Fatal("unknown trigger was accepted")
	}
	if validSenderSessionTerminalPair(SenderSessionTerminalTriggerGracefulStop, "unknown") {
		t.Fatal("unknown provenance was accepted")
	}
	if (SenderSessionTerminated{
		Trigger:    SenderSessionTerminalTriggerGracefulStop,
		Provenance: SenderSessionTerminalProvenanceNormalStop,
	}).Valid() {
		t.Fatal("zero protocol session identity was accepted")
	}
}

func TestSenderRuntimeLifecycleEmitsOneExplicitTerminalRoot(t *testing.T) {
	for _, test := range []struct {
		name       string
		terminate  func(*SenderRuntime) error
		trigger    SenderSessionTerminalTrigger
		provenance SenderSessionTerminalProvenance
	}{
		{
			name: "graceful stop",
			terminate: func(runtime *SenderRuntime) error {
				if err := runtime.BeginStop(context.Background(), "test stop"); err != nil {
					return err
				}
				return runtime.WaitStopped(context.Background())
			},
			trigger:    SenderSessionTerminalTriggerGracefulStop,
			provenance: SenderSessionTerminalProvenanceNormalStop,
		},
		{
			name: "forced close",
			terminate: func(runtime *SenderRuntime) error {
				runtime.BeginClose()
				runtime.WaitClosed()
				return nil
			},
			trigger:    SenderSessionTerminalTriggerForcedClose,
			provenance: SenderSessionTerminalProvenanceCallerStop,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newVerticalFixture(t)
			observed := make(chan SenderSessionTerminated, 2)
			fixture.senderFactory.terminalObserver = newSenderTerminalObservers(
				nil,
				SenderSessionTerminalObserverFunc(func(event SenderSessionTerminated) {
					observed <- event
				}),
			)
			sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
			t.Cleanup(receiver.Close)
			t.Cleanup(sender.Close)

			if err := test.terminate(sender); err != nil {
				t.Fatalf("terminate sender: %v", err)
			}
			event := awaitSenderSessionTermination(t, observed)
			assertSenderSessionTermination(
				t,
				event,
				sender.sessionID,
				test.trigger,
				test.provenance,
			)
			assertNoSenderSessionTermination(t, observed)
		})
	}
}

func TestSenderTerminalPumpCausePrecedesLaneCompletion(t *testing.T) {
	for _, test := range []struct {
		name       string
		pumpError  error
		trigger    SenderSessionTerminalTrigger
		provenance SenderSessionTerminalProvenance
	}{
		{
			name:       "authenticated peer terminal",
			pumpError:  protocolsession.ErrPeerSessionTerminal,
			trigger:    SenderSessionTerminalTriggerPeerTerminal,
			provenance: SenderSessionTerminalProvenanceRemoteClose,
		},
		{
			name:       "local pump failure",
			pumpError:  errors.New("authenticated dispatch failed"),
			trigger:    SenderSessionTerminalTriggerRuntimeFailed,
			provenance: SenderSessionTerminalProvenanceLocalFault,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
			runtime.lanes.mu.Lock()
			lane := runtime.lanes.active[runtime.initial.ID]
			runtime.lanes.mu.Unlock()
			if lane == nil {
				t.Fatal("initial lane owner is missing")
			}
			observed := make(chan SenderSessionTerminated, 1)
			runtime.sessionTerminalObserver = SenderSessionTerminalObserverFunc(
				func(event SenderSessionTerminated) {
					select {
					case <-lane.done:
						t.Error("terminal root was emitted after lane completion")
					default:
					}
					observed <- event
				},
			)
			runtime.lanes.markClosing(lane)
			runtime.lanes.settleRun(
				lane,
				laneRunResult{component: "pump", err: test.pumpError},
				laneRunResult{component: "writer", err: context.Canceled},
			)

			event := awaitSenderSessionTermination(t, observed)
			assertSenderSessionTermination(
				t,
				event,
				runtime.sessionID,
				test.trigger,
				test.provenance,
			)
			if !errors.Is(runtime.Err(), test.pumpError) {
				t.Fatalf("runtime error=%v, want pump cause %v", runtime.Err(), test.pumpError)
			}
			select {
			case <-lane.done:
			default:
				t.Fatal("lane completion was not published")
			}
		})
	}
}

func TestSenderTerminalLastLaneRetirementClaimsBeforeCompletion(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	runtime.lanes.mu.Lock()
	lane := runtime.lanes.active[runtime.initial.ID]
	runtime.lanes.mu.Unlock()
	if lane == nil {
		t.Fatal("initial lane owner is missing")
	}
	observed := make(chan SenderSessionTerminated, 1)
	runtime.sessionTerminalObserver = SenderSessionTerminalObserverFunc(
		func(event SenderSessionTerminated) {
			select {
			case <-lane.done:
				t.Error("path root was emitted after lane completion")
			default:
			}
			observed <- event
		},
	)

	runtime.lanes.completeLane(lane)

	event := awaitSenderSessionTermination(t, observed)
	assertSenderSessionTermination(
		t,
		event,
		runtime.sessionID,
		SenderSessionTerminalTriggerPathsExhausted,
		SenderSessionTerminalProvenanceLaneRetirement,
	)
	if runtime.Err() != nil {
		t.Fatalf("ordinary path retirement changed product error: %v", runtime.Err())
	}
}

func TestSenderTerminalSurvivingLanePreventsSessionTermination(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	replacementIdentity := LaneIdentity{ID: 2, Epoch: 1}
	if _, err := runtime.lanes.add(
		replacementIdentity,
		newMemoryChannel(t),
		permissiveInboundAuthenticator(),
		false,
	); err != nil {
		t.Fatal(err)
	}
	observed := make(chan SenderSessionTerminated, 1)
	runtime.sessionTerminalObserver = SenderSessionTerminalObserverFunc(
		func(event SenderSessionTerminated) { observed <- event },
	)
	runtime.lanes.mu.Lock()
	initial := runtime.lanes.active[runtime.initial.ID]
	runtime.lanes.mu.Unlock()
	runtime.lanes.completeLane(initial)

	assertNoSenderSessionTermination(t, observed)
	if runtime.ctx.Err() != nil {
		t.Fatalf("surviving replacement lane did not preserve session: %v", runtime.ctx.Err())
	}
	runtime.sessionTerminalObserver = nil
}

func TestSenderTerminalArbiterPreservesFirstCauseAndSeparatesSendConsequence(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	roots := make(chan SenderSessionTerminated, 3)
	runtime.sessionTerminalObserver = SenderSessionTerminalObserverFunc(
		func(event SenderSessionTerminated) { roots <- event },
	)
	sendCount := atomic.Uint32{}
	sendObserver := SenderTerminalSendObserverFunc(func(SenderTerminalSendObserved) {
		sendCount.Add(1)
		panic("diagnostic send observer failed")
	})

	graceful := runtime.claimTermination(runtimeTerminationGracefulStop)
	runtime.publishTermination(graceful)
	observeSenderTerminalSend(
		sendObserver,
		runtime.sessionID,
		runtime.initial,
		protocolsession.SendCompletion{
			Settled:              true,
			TransportDisposition: framechannel.SendAccepted,
			Outcome:              protocolsession.SendOutcomeDropped,
			Err:                  errors.New("terminal delivery failed"),
		},
		false,
	)
	runtime.terminate(runtimeTerminationFailed)
	runtime.terminate(runtimeTerminationForcedClose)

	event := awaitSenderSessionTermination(t, roots)
	assertSenderSessionTermination(
		t,
		event,
		runtime.sessionID,
		SenderSessionTerminalTriggerGracefulStop,
		SenderSessionTerminalProvenanceNormalStop,
	)
	assertNoSenderSessionTermination(t, roots)
	if sendCount.Load() != 1 {
		t.Fatalf("terminal-send consequences=%d, want 1", sendCount.Load())
	}
	if runtime.ctx.Err() == nil {
		t.Fatal("later failure did not cancel the already-claimed graceful session")
	}
}

func TestSenderTerminalArbiterConcurrentClaimsEmitExactlyOneValidRoot(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	observed := make(chan SenderSessionTerminated, 5)
	runtime.sessionTerminalObserver = SenderSessionTerminalObserverFunc(
		func(event SenderSessionTerminated) { observed <- event },
	)
	causes := []runtimeTerminationCause{
		runtimeTerminationGracefulStop,
		runtimeTerminationForcedClose,
		runtimeTerminationPeerTerminal,
		runtimeTerminationPathsExhausted,
		runtimeTerminationFailed,
	}
	start := make(chan struct{})
	var wait sync.WaitGroup
	for _, cause := range causes {
		wait.Go(func() {
			<-start
			runtime.terminate(cause)
		})
	}
	close(start)
	wait.Wait()

	event := awaitSenderSessionTermination(t, observed)
	if !event.Valid() {
		t.Fatalf("concurrent terminal root is invalid: %+v", event)
	}
	assertNoSenderSessionTermination(t, observed)
}

func TestSenderTerminalObserverPanicCannotChangeRuntimeOutcome(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	runtime.sessionTerminalObserver = SenderSessionTerminalObserverFunc(
		func(SenderSessionTerminated) { panic("diagnostic root observer failed") },
	)

	runtime.terminate(runtimeTerminationForcedClose)

	if runtime.ctx.Err() == nil {
		t.Fatal("observer panic prevented lifecycle cancellation")
	}
	if runtime.Err() != nil {
		t.Fatalf("observer panic changed product error: %v", runtime.Err())
	}
}

func awaitSenderSessionTermination(
	t *testing.T,
	observed <-chan SenderSessionTerminated,
) SenderSessionTerminated {
	t.Helper()
	select {
	case event := <-observed:
		return event
	case <-time.After(time.Second):
		t.Fatal("sender session terminal root was not observed")
		return SenderSessionTerminated{}
	}
}

func assertNoSenderSessionTermination(
	t *testing.T,
	observed <-chan SenderSessionTerminated,
) {
	t.Helper()
	select {
	case event := <-observed:
		t.Fatalf("unexpected additional sender session terminal root: %+v", event)
	default:
	}
}

func assertSenderSessionTermination(
	t *testing.T,
	event SenderSessionTerminated,
	sessionID protocolsession.ProtocolSessionID,
	trigger SenderSessionTerminalTrigger,
	provenance SenderSessionTerminalProvenance,
) {
	t.Helper()
	if !event.Valid() || event.ProtocolSessionID != sessionID ||
		event.Trigger != trigger || event.Provenance != provenance {
		t.Fatalf(
			"sender session terminal=%+v, want session=%v trigger=%q provenance=%q",
			event,
			sessionID,
			trigger,
			provenance,
		)
	}
}
