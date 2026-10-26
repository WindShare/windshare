package sessioncore

import (
	"context"
	"errors"
	"fmt"
	"github.com/windshare/windshare/core/framechannel"
	sessioncontract "github.com/windshare/windshare/core/session"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSenderTerminalPumpCausePrecedesLaneCompletion(t *testing.T) {
	for _, test := range []struct {
		name       string
		pumpError  error
		trigger    sessionobs.SenderSessionTerminalTrigger
		provenance sessionobs.SenderSessionTerminalProvenance
	}{
		{
			name:       "authenticated peer terminal",
			pumpError:  protocolsession.ErrPeerSessionTerminal,
			trigger:    sessionobs.SenderSessionTerminalTriggerPeerTerminal,
			provenance: sessionobs.SenderSessionTerminalProvenanceRemoteClose,
		},
		{
			name:       "local pump failure",
			pumpError:  errors.New("authenticated dispatch failed"),
			trigger:    sessionobs.SenderSessionTerminalTriggerRuntimeFailed,
			provenance: sessionobs.SenderSessionTerminalProvenanceLocalFault,
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
			observed := make(chan sessionobs.SenderSessionTerminated, 1)
			runtime.sessionTerminalObserver = sessionobs.SenderSessionTerminalObserverFunc(
				func(event sessionobs.SenderSessionTerminated) {
					select {
					case <-lane.done:
						t.Error("terminal root was emitted after lane completion")
					default:
					}
					observed <- event
				},
			)
			runtime.lanes.markClosing(lane)
			runtime.lanes.retireRun(
				lane,
				laneRunResult{component: "pump", err: test.pumpError},
				laneRunResult{component: "writer", err: context.Canceled},
			)
			runtime.lanes.completeLane(lane)

			event := awaitSenderSessionTermination(t, observed)
			assertSenderSessionTermination(
				t,
				event,
				runtime.sessionID,
				test.trigger,
				test.provenance,
			)
			nodes := event.Failure.Nodes()
			if event.Failure.Source() != "lane_pump" || len(nodes) != 2 || nodes[1].Message != test.pumpError.Error() {
				t.Fatalf("pump failure detail was lost: %#v", event.Failure)
			}
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
	observed := make(chan sessionobs.SenderSessionTerminated, 1)
	runtime.sessionTerminalObserver = sessionobs.SenderSessionTerminalObserverFunc(
		func(event sessionobs.SenderSessionTerminated) {
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
		sessionobs.SenderSessionTerminalTriggerPathsExhausted,
		sessionobs.SenderSessionTerminalProvenanceLaneRetirement,
	)
	if runtime.Err() != nil {
		t.Fatalf("ordinary path retirement changed product error: %v", runtime.Err())
	}
}

func TestSenderTerminalSurvivingLanePreventsSessionTermination(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	replacementIdentity := sessioncontract.LaneIdentity{ID: 2, Epoch: 1}
	if _, err := runtime.lanes.add(
		replacementIdentity,
		newMemoryChannel(t),
		permissiveInboundAuthenticator(),
		false,
	); err != nil {
		t.Fatal(err)
	}
	observed := make(chan sessionobs.SenderSessionTerminated, 1)
	runtime.sessionTerminalObserver = sessionobs.SenderSessionTerminalObserverFunc(
		func(event sessionobs.SenderSessionTerminated) { observed <- event },
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
	roots := make(chan sessionobs.SenderSessionTerminated, 3)
	runtime.sessionTerminalObserver = sessionobs.SenderSessionTerminalObserverFunc(
		func(event sessionobs.SenderSessionTerminated) { roots <- event },
	)
	sendCount := atomic.Uint32{}
	sendObserver := sessionobs.SenderTerminalSendObserverFunc(func(sessionobs.SenderTerminalSendObserved) {
		sendCount.Add(1)
		panic("diagnostic send observer failed")
	})

	graceful := runtime.claimTermination(RuntimeTerminationGracefulStop)
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
	runtime.terminate(RuntimeTerminationForcedClose)

	event := awaitSenderSessionTermination(t, roots)
	assertSenderSessionTermination(
		t,
		event,
		runtime.sessionID,
		sessionobs.SenderSessionTerminalTriggerGracefulStop,
		sessionobs.SenderSessionTerminalProvenanceNormalStop,
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
	observed := make(chan sessionobs.SenderSessionTerminated, 5)
	runtime.sessionTerminalObserver = sessionobs.SenderSessionTerminalObserverFunc(
		func(event sessionobs.SenderSessionTerminated) { observed <- event },
	)
	causes := []runtimeTerminationCause{
		RuntimeTerminationGracefulStop,
		RuntimeTerminationForcedClose,
		runtimeTerminationPeerTerminal,
		RuntimeTerminationPathsExhausted,
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
	runtime.sessionTerminalObserver = sessionobs.SenderSessionTerminalObserverFunc(
		func(sessionobs.SenderSessionTerminated) { panic("diagnostic root observer failed") },
	)

	runtime.terminate(RuntimeTerminationForcedClose)

	if runtime.ctx.Err() == nil {
		t.Fatal("observer panic prevented lifecycle cancellation")
	}
	if runtime.Err() != nil {
		t.Fatalf("observer panic changed product error: %v", runtime.Err())
	}
}

func awaitSenderSessionTermination(
	t *testing.T,
	observed <-chan sessionobs.SenderSessionTerminated,
) sessionobs.SenderSessionTerminated {
	t.Helper()
	select {
	case event := <-observed:
		return event
	case <-time.After(time.Second):
		t.Fatal("sender session terminal root was not observed")
		return sessionobs.SenderSessionTerminated{}
	}
}

func assertNoSenderSessionTermination(
	t *testing.T,
	observed <-chan sessionobs.SenderSessionTerminated,
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
	event sessionobs.SenderSessionTerminated,
	sessionID protocolsession.ProtocolSessionID,
	trigger sessionobs.SenderSessionTerminalTrigger,
	provenance sessionobs.SenderSessionTerminalProvenance,
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

func TestSenderTerminationSnapshotKeepsWinningErrorBeforeCleanup(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	observed := make(chan sessionobs.SenderSessionTerminated, 2)
	runtime.sessionTerminalObserver = sessionobs.SenderSessionTerminalObserverFunc(func(event sessionobs.SenderSessionTerminated) { observed <- event })
	root := errors.New("original native failure")
	runtime.terminateRuntimeFailed(fmt.Errorf("peer response: %w", root))
	runtime.terminateRuntimeFailed(errors.New("later cleanup failure"))
	event := awaitSenderSessionTermination(t, observed)
	nodes := event.Failure.Nodes()
	if event.Trigger != sessionobs.SenderSessionTerminalTriggerRuntimeFailed || len(nodes) != 2 ||
		nodes[1].Message != root.Error() || nodes[1].Parent != 0 {
		t.Fatalf("wrong terminal error snapshot: %#v", event)
	}
	foundCaller := false
	for _, frame := range event.Failure.CaptureStack() {
		foundCaller = foundCaller || strings.HasSuffix(frame.Function, ".TestSenderTerminationSnapshotKeepsWinningErrorBeforeCleanup")
	}
	if !foundCaller {
		t.Fatal("terminal snapshot lost failure call site")
	}
	assertNoSenderSessionTermination(t, observed)
	if !errors.Is(runtime.Err(), root) || runtime.ctx.Err() == nil {
		t.Fatal("diagnostics changed lifecycle outcome")
	}
}

type terminalInspectionError struct {
	calls              *atomic.Uint32
	canceled           <-chan struct{}
	beforeCancellation *atomic.Bool
}

func (err terminalInspectionError) Error() string {
	err.calls.Add(1)
	select {
	case <-err.canceled:
	default:
		err.beforeCancellation.Store(true)
	}
	panic("custom Error method failed")
}

func TestSenderTerminationInspectionCannotPreventCancellationOrRunWithoutObserver(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
		observed := make(chan sessionobs.SenderSessionTerminated, 1)
		if enabled {
			runtime.sessionTerminalObserver = sessionobs.SenderSessionTerminalObserverFunc(func(event sessionobs.SenderSessionTerminated) { observed <- event })
		}
		calls := &atomic.Uint32{}
		beforeCancellation := &atomic.Bool{}
		runtime.terminateRuntimeFailed(terminalInspectionError{calls: calls, canceled: runtime.ctx.Done(), beforeCancellation: beforeCancellation})
		if runtime.ctx.Err() == nil || beforeCancellation.Load() {
			t.Fatal("error inspection preceded cancellation")
		}
		if enabled {
			event := awaitSenderSessionTermination(t, observed)
			if !event.Failure.Nodes()[0].InspectionFailed || calls.Load() != 1 {
				t.Fatal("inspection panic was not isolated")
			}
		} else if calls.Load() != 0 {
			t.Fatal("disabled diagnostics inspected the error")
		}
	}
}

func TestSenderTerminationLosingFailureCannotAddEvidenceToGracefulRoot(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	observed := make(chan sessionobs.SenderSessionTerminated, 1)
	runtime.sessionTerminalObserver = sessionobs.SenderSessionTerminalObserverFunc(func(event sessionobs.SenderSessionTerminated) { observed <- event })
	claim := runtime.claimTermination(RuntimeTerminationGracefulStop)
	calls := &atomic.Uint32{}
	beforeCancellation := &atomic.Bool{}
	runtime.terminateRuntimeFailed(terminalInspectionError{calls: calls, canceled: runtime.ctx.Done(), beforeCancellation: beforeCancellation})
	runtime.publishTermination(claim)
	event := awaitSenderSessionTermination(t, observed)
	if event.Failure.Present() || calls.Load() != 0 || event.Trigger != sessionobs.SenderSessionTerminalTriggerGracefulStop {
		t.Fatal("losing failure contaminated the graceful root")
	}
}
