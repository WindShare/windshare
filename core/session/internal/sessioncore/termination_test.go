package sessioncore

import (
	"context"
	"errors"
	"fmt"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTerminationPumpCausePrecedesLaneCompletion(t *testing.T) {
	for _, test := range []struct {
		name      string
		pumpError error
		cause     TerminationCause
	}{
		{
			name:      "authenticated peer terminal",
			pumpError: protocolsession.ErrPeerSessionTerminal,
			cause:     TerminationPeerTerminal,
		},
		{
			name:      "local pump failure",
			pumpError: errors.New("authenticated dispatch failed"),
			cause:     TerminationFailed,
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
			observed := make(chan Termination, 1)
			runtime.terminationObserver = TerminationObserverFunc(
				func(event Termination) {
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

			event := awaitTermination(t, observed)
			assertTermination(
				t,
				event,
				runtime.sessionID,
				test.cause,
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

func TestTerminationLastLaneRetirementClaimsBeforeCompletion(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	runtime.lanes.mu.Lock()
	lane := runtime.lanes.active[runtime.initial.ID]
	runtime.lanes.mu.Unlock()
	if lane == nil {
		t.Fatal("initial lane owner is missing")
	}
	observed := make(chan Termination, 1)
	runtime.terminationObserver = TerminationObserverFunc(
		func(event Termination) {
			select {
			case <-lane.done:
				t.Error("path root was emitted after lane completion")
			default:
			}
			observed <- event
		},
	)

	runtime.lanes.completeLane(lane)

	event := awaitTermination(t, observed)
	assertTermination(
		t,
		event,
		runtime.sessionID,
		TerminationPathsExhausted,
	)
	if runtime.Err() != nil {
		t.Fatalf("ordinary path retirement changed product error: %v", runtime.Err())
	}
}

func TestTerminationSurvivingLanePreventsSessionTermination(t *testing.T) {
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
	observed := make(chan Termination, 1)
	runtime.terminationObserver = TerminationObserverFunc(
		func(event Termination) { observed <- event },
	)
	runtime.lanes.mu.Lock()
	initial := runtime.lanes.active[runtime.initial.ID]
	runtime.lanes.mu.Unlock()
	runtime.lanes.completeLane(initial)

	assertNoTermination(t, observed)
	if runtime.ctx.Err() != nil {
		t.Fatalf("surviving replacement lane did not preserve session: %v", runtime.ctx.Err())
	}
	runtime.terminationObserver = nil
}

func TestTerminationArbiterPreservesFirstCause(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	roots := make(chan Termination, 3)
	runtime.terminationObserver = TerminationObserverFunc(
		func(event Termination) { roots <- event },
	)

	graceful := runtime.claimTermination(TerminationGracefulStop)
	graceful.Publish()

	runtime.terminate(TerminationFailed)
	runtime.terminate(TerminationForcedClose)

	event := awaitTermination(t, roots)
	assertTermination(
		t,
		event,
		runtime.sessionID,
		TerminationGracefulStop,
	)
	assertNoTermination(t, roots)
	if runtime.ctx.Err() == nil {
		t.Fatal("later failure did not cancel the already-claimed graceful session")
	}
}

func TestTerminationArbiterConcurrentClaimsEmitExactlyOneValidRoot(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	observed := make(chan Termination, 5)
	runtime.terminationObserver = TerminationObserverFunc(
		func(event Termination) { observed <- event },
	)
	causes := []TerminationCause{
		TerminationGracefulStop,
		TerminationForcedClose,
		TerminationPeerTerminal,
		TerminationPathsExhausted,
		TerminationFailed,
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

	event := awaitTermination(t, observed)
	if !event.Cause.valid() {
		t.Fatalf("concurrent terminal root is invalid: %+v", event)
	}
	assertNoTermination(t, observed)
}

func TestTerminationObserverPanicCannotChangeRuntimeOutcome(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	runtime.terminationObserver = TerminationObserverFunc(
		func(Termination) { panic("diagnostic root observer failed") },
	)

	runtime.terminate(TerminationForcedClose)

	if runtime.ctx.Err() == nil {
		t.Fatal("observer panic prevented lifecycle cancellation")
	}
	if runtime.Err() != nil {
		t.Fatalf("observer panic changed product error: %v", runtime.Err())
	}
}

func awaitTermination(
	t *testing.T,
	observed <-chan Termination,
) Termination {
	t.Helper()
	select {
	case event := <-observed:
		return event
	case <-time.After(time.Second):
		t.Fatal("session terminal root was not observed")
		return Termination{}
	}
}

func assertNoTermination(
	t *testing.T,
	observed <-chan Termination,
) {
	t.Helper()
	select {
	case event := <-observed:
		t.Fatalf("unexpected additional session terminal root: %+v", event)
	default:
	}
}

func assertTermination(
	t *testing.T,
	event Termination,
	sessionID protocolsession.ProtocolSessionID,
	cause TerminationCause,
) {
	t.Helper()
	if !event.Cause.valid() || event.ProtocolSessionID != sessionID || event.Cause != cause {
		t.Fatalf("termination=%+v, want session=%v cause=%v", event, sessionID, cause)
	}
}

func TestTerminationSnapshotKeepsWinningErrorBeforeCleanup(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	observed := make(chan Termination, 2)
	runtime.terminationObserver = TerminationObserverFunc(func(event Termination) { observed <- event })
	root := errors.New("original native failure")
	runtime.terminateRuntimeFailed(fmt.Errorf("peer response: %w", root))
	runtime.terminateRuntimeFailed(errors.New("later cleanup failure"))
	event := awaitTermination(t, observed)
	nodes := event.Failure.Nodes()
	if event.Cause != TerminationFailed || len(nodes) != 2 ||
		nodes[1].Message != root.Error() || nodes[1].Parent != 0 {
		t.Fatalf("wrong terminal error snapshot: %#v", event)
	}
	foundCaller := false
	for _, frame := range event.Failure.CaptureStack() {
		foundCaller = foundCaller || strings.HasSuffix(frame.Function, ".TestTerminationSnapshotKeepsWinningErrorBeforeCleanup")
	}
	if !foundCaller {
		t.Fatal("terminal snapshot lost failure call site")
	}
	assertNoTermination(t, observed)
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

func TestTerminationInspectionCannotPreventCancellationOrRunWithoutObserver(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
		observed := make(chan Termination, 1)
		if enabled {
			runtime.terminationObserver = TerminationObserverFunc(func(event Termination) { observed <- event })
		}
		calls := &atomic.Uint32{}
		beforeCancellation := &atomic.Bool{}
		runtime.terminateRuntimeFailed(terminalInspectionError{calls: calls, canceled: runtime.ctx.Done(), beforeCancellation: beforeCancellation})
		if runtime.ctx.Err() == nil || beforeCancellation.Load() {
			t.Fatal("error inspection preceded cancellation")
		}
		if enabled {
			event := awaitTermination(t, observed)
			if !event.Failure.Nodes()[0].InspectionFailed || calls.Load() != 1 {
				t.Fatal("inspection panic was not isolated")
			}
		} else if calls.Load() != 0 {
			t.Fatal("disabled diagnostics inspected the error")
		}
	}
}

func TestTerminationLosingFailureCannotAddEvidenceToGracefulRoot(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	observed := make(chan Termination, 1)
	runtime.terminationObserver = TerminationObserverFunc(func(event Termination) { observed <- event })
	claim := runtime.claimTermination(TerminationGracefulStop)
	calls := &atomic.Uint32{}
	beforeCancellation := &atomic.Bool{}
	runtime.terminateRuntimeFailed(terminalInspectionError{calls: calls, canceled: runtime.ctx.Done(), beforeCancellation: beforeCancellation})
	claim.Publish()
	event := awaitTermination(t, observed)
	if event.Failure.Present() || calls.Load() != 0 || event.Cause != TerminationGracefulStop {
		t.Fatal("losing failure contaminated the graceful root")
	}
}
