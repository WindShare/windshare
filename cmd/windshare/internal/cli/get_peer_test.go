package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
)

type cliReceiverPeerAttempt struct {
	ready     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	err       error
	outcome   receiverPeerMonitorOutcome
	lane      sessionruntime.LaneIdentity
}

func newCLIReceiverPeerAttempt() *cliReceiverPeerAttempt {
	return &cliReceiverPeerAttempt{ready: make(chan struct{}), done: make(chan struct{})}
}

func (attempt *cliReceiverPeerAttempt) Ready() <-chan struct{} { return attempt.ready }
func (attempt *cliReceiverPeerAttempt) Done() <-chan struct{}  { return attempt.done }
func (attempt *cliReceiverPeerAttempt) Err() error             { return attempt.err }
func (attempt *cliReceiverPeerAttempt) Outcome() receiverPeerMonitorOutcome {
	return attempt.outcome
}
func (attempt *cliReceiverPeerAttempt) Lane() (sessionruntime.LaneIdentity, bool) {
	return attempt.lane, attempt.lane.ID != 0 && attempt.lane.Epoch != 0
}
func (attempt *cliReceiverPeerAttempt) Close() error {
	attempt.closeOnce.Do(func() {
		attempt.outcome = receiverPeerMonitorOutcome{
			disposition: receiverPeerLocalStop,
		}
		close(attempt.done)
	})
	return nil
}
func (attempt *cliReceiverPeerAttempt) finish(err error) {
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition:   receiverPeerFallbackAllowed,
		retainedCause: err,
	})
}

func (attempt *cliReceiverPeerAttempt) finishOutcome(outcome receiverPeerMonitorOutcome) {
	attempt.closeOnce.Do(func() {
		attempt.err = outcome.retainedCause
		attempt.outcome = outcome
		close(attempt.done)
	})
}

type cliReceiverRuntimeCloser struct{ calls atomic.Int32 }

func (runtime *cliReceiverRuntimeCloser) Close() { runtime.calls.Add(1) }

func TestReceiverPeerSetupFailureLogsSafePhaseAndCauseClass(t *testing.T) {
	var stderr bytes.Buffer
	app := &App{
		Stderr: &stderr,
		receiverPeerFactory: func() (receiverPeerStarter, error) {
			return nil, v2peer.ErrNegotiation
		},
	}
	var signal receiverPeerSignal
	peer := app.startReceiverPeer(context.Background(), nil, func(observed receiverPeerSignal) {
		signal = observed
	})
	if peer != nil || signal != receiverPeerFailed {
		t.Fatalf("setup failure peer=%v signal=%v", peer, signal)
	}
	if diagnostic := stderr.String(); !strings.Contains(diagnostic, "phase=factory") ||
		!strings.Contains(diagnostic, "cause_class=negotiation") {
		t.Fatalf("setup failure diagnostic=%q", diagnostic)
	}
}

func TestReceiverPeerSetupFailureDistinguishesEveryPhase(t *testing.T) {
	for _, test := range []struct {
		phase receiverPeerSetupPhase
		cause error
		class v2peer.ReceiverCauseClass
	}{
		{phase: receiverPeerSetupFactory, cause: v2peer.ErrNegotiation, class: v2peer.ReceiverCauseNegotiation},
		{phase: receiverPeerSetupSignaling, cause: v2peer.ErrConfig, class: v2peer.ReceiverCauseConfiguration},
		{phase: receiverPeerSetupStart, cause: context.DeadlineExceeded, class: v2peer.ReceiverCauseDeadline},
	} {
		t.Run(string(test.phase), func(t *testing.T) {
			var stderr bytes.Buffer
			(&App{Stderr: &stderr}).logReceiverPeerSetupFailure(test.phase, test.cause)
			diagnostic := stderr.String()
			if !strings.Contains(diagnostic, "phase="+string(test.phase)) ||
				!strings.Contains(diagnostic, "cause_class="+string(test.class)) {
				t.Fatalf("setup diagnostic=%q", diagnostic)
			}
		})
	}
}

type cyclicReceiverSetupError struct{}

func (*cyclicReceiverSetupError) Error() string { return "cyclic setup failure" }
func (failure *cyclicReceiverSetupError) Unwrap() error {
	return failure
}

func TestReceiverPeerSetupCauseClassificationIsCycleBounded(t *testing.T) {
	if class := receiverPeerSetupCauseClass(&cyclicReceiverSetupError{}); class != v2peer.ReceiverCauseUnknown {
		t.Fatalf("cyclic setup cause class=%s", class)
	}
}

func TestReceiverPeerTerminationTraceIsDrainedSynchronously(t *testing.T) {
	var stderr bytes.Buffer
	app := &App{Stderr: &stderr}
	traces := make(chan receiverPeerTerminationTrace, 1)
	traces <- receiverPeerTerminationTrace{
		operationID:           protocolsession.OperationID{1},
		localGeneration:       7,
		transitionAuthority:   v2peer.ReceiverTerminalRemote,
		transitionProvenance:  v2peer.ReceiverProvenanceRemoteOperationRejected,
		disposition:           v2peer.ReceiverDispositionFallbackAllowed,
		consequenceProvenance: v2peer.ReceiverProvenanceRemoteOperationRejected,
		diagnosticsTruncated:  true,
		retainedCauseClasses:  []v2peer.ReceiverCauseClass{v2peer.ReceiverCauseProtocol},
		teardownTransitions: []v2peer.PeerTeardownTransition{
			v2peer.PeerTeardownPeerShutdownInitiated,
			v2peer.PeerTeardownPeerShutdownReturned,
			v2peer.PeerTeardownChannelDrainStarted,
			v2peer.PeerTeardownChannelDrainJoined,
		},
		peerShutdownFailed: false,
		channelDrainFailed: true,
	}

	app.awaitReceiverTerminationTrace(traces)

	diagnostic := stderr.String()
	if !strings.Contains(diagnostic, "local_generation=7") ||
		!strings.Contains(diagnostic, "transition_authority=remote") ||
		!strings.Contains(diagnostic, "transition_provenance=remote_operation_rejected") ||
		!strings.Contains(diagnostic, "disposition=fallback_allowed") ||
		!strings.Contains(diagnostic, "diagnostics_truncated=true") ||
		!strings.Contains(diagnostic, "protocol") ||
		!strings.Contains(diagnostic, "teardown_transitions=[peer_shutdown_initiated peer_shutdown_returned channel_drain_started channel_drain_joined]") ||
		!strings.Contains(diagnostic, "peer_shutdown_failed=false") ||
		!strings.Contains(diagnostic, "channel_drain_failed=true") {
		t.Fatalf("termination trace diagnostic=%q", diagnostic)
	}
}

func TestReceiverPeerMonitorClosesSessionForAuthenticatedAuthorityViolation(t *testing.T) {
	var stderr bytes.Buffer
	app := &App{Stderr: &stderr}
	attempt := newCLIReceiverPeerAttempt()
	runtime := &cliReceiverRuntimeCloser{}
	fatalCause := errors.New("binding substitution")
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition:   receiverPeerSessionUnsafe,
		retainedCause: fatalCause,
	})
	var signal receiverPeerSignal

	app.monitorReceiverPeer(attempt, runtime, func(observed receiverPeerSignal) { signal = observed })

	if runtime.calls.Load() != 1 {
		t.Fatalf("runtime close calls = %d", runtime.calls.Load())
	}
	if !strings.Contains(stderr.String(), "closing the session") {
		t.Fatalf("fatal diagnostic = %q", stderr.String())
	}
	if signal != receiverPeerSessionFatal {
		t.Fatalf("fatal signal=%v", signal)
	}
}

func TestReceiverPeerMonitorKeepsRelaySessionForAttemptLocalFailure(t *testing.T) {
	var stderr bytes.Buffer
	app := &App{Stderr: &stderr}
	attempt := newCLIReceiverPeerAttempt()
	runtime := &cliReceiverRuntimeCloser{}
	attempt.finish(errors.New("ICE negotiation failed"))
	var signal receiverPeerSignal

	app.monitorReceiverPeer(attempt, runtime, func(observed receiverPeerSignal) { signal = observed })

	if runtime.calls.Load() != 0 {
		t.Fatal("attempt-local peer failure closed the relay session")
	}
	if !strings.Contains(stderr.String(), "continuing through relay") {
		t.Fatalf("fallback diagnostic = %q", stderr.String())
	}
	if signal != receiverPeerFailed {
		t.Fatalf("fallback signal=%v", signal)
	}
}

func TestReceiverPeerMonitorRetainsJoinedCancellationFailure(t *testing.T) {
	var stderr bytes.Buffer
	app := &App{Stderr: &stderr}
	attempt := newCLIReceiverPeerAttempt()
	runtime := &cliReceiverRuntimeCloser{}
	retained := errors.New("ICE teardown failed")
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition:   receiverPeerFallbackAllowed,
		retainedCause: retained,
	})
	var signal receiverPeerSignal

	app.monitorReceiverPeer(attempt, runtime, func(observed receiverPeerSignal) { signal = observed })

	if signal != receiverPeerFailed {
		t.Fatalf("joined cancellation residual signal=%v", signal)
	}
	if runtime.calls.Load() != 0 {
		t.Fatal("attempt-local residual closed the relay session")
	}
	if !strings.Contains(stderr.String(), "continuing through relay") {
		t.Fatalf("joined cancellation residual diagnostic=%q", stderr.String())
	}
}

func TestReceiverPeerMonitorSignalsReadyThenCleanDetach(t *testing.T) {
	var stderr bytes.Buffer
	app := &App{Stderr: &stderr}
	attempt := newCLIReceiverPeerAttempt()
	attempt.lane = sessionruntime.LaneIdentity{ID: 3, Epoch: 1}
	runtime := &cliReceiverRuntimeCloser{}
	signals := make(chan receiverPeerSignal, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.monitorReceiverPeer(attempt, runtime, func(signal receiverPeerSignal) { signals <- signal })
	}()
	close(attempt.ready)
	if signal := <-signals; signal != receiverPeerReady {
		t.Fatalf("ready signal=%v", signal)
	}
	attempt.finish(nil)
	if signal := <-signals; signal != receiverPeerDetached {
		t.Fatalf("detach signal=%v", signal)
	}
	<-done
	if runtime.calls.Load() != 0 {
		t.Fatal("clean peer detach closed the authenticated relay session")
	}
	if !strings.Contains(stderr.String(), "direct peer lane active") || !strings.Contains(stderr.String(), "lane lost") {
		t.Fatalf("peer lifecycle diagnostic=%q", stderr.String())
	}
}

func TestActiveReceiverPeerCloseJoinsItsMonitor(t *testing.T) {
	attempt := newCLIReceiverPeerAttempt()
	monitorDone := make(chan struct{})
	go func() {
		<-attempt.Done()
		close(monitorDone)
	}()
	peer := &activeReceiverPeer{attempt: attempt, done: monitorDone}

	peer.Close()
	peer.Close()

	select {
	case <-monitorDone:
	default:
		t.Fatal("peer cleanup returned before its monitor finished")
	}
}

func TestReceiverPeerMonitorKeepsCleanLocalCloseSilent(t *testing.T) {
	attempt := newCLIReceiverPeerAttempt()
	runtime := &cliReceiverRuntimeCloser{}
	signals := make(chan receiverPeerSignal, 1)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		(&App{}).monitorReceiverPeer(attempt, runtime, func(signal receiverPeerSignal) {
			signals <- signal
		})
	}()

	if err := attempt.Close(); err != nil {
		t.Fatal(err)
	}
	<-monitorDone
	select {
	case signal := <-signals:
		t.Fatalf("clean local Close emitted peer signal=%v", signal)
	default:
	}
	if runtime.calls.Load() != 0 {
		t.Fatal("clean local Close ended the relay session")
	}
}

func TestReceiverPeerMonitorTreatsBenignRemoteFinalAsPathFailure(t *testing.T) {
	attempt := newCLIReceiverPeerAttempt()
	runtime := &cliReceiverRuntimeCloser{}
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition: receiverPeerFallbackAllowed,
	})
	var signal receiverPeerSignal

	(&App{}).monitorReceiverPeer(attempt, runtime, func(observed receiverPeerSignal) {
		signal = observed
	})

	if signal != receiverPeerFailed {
		t.Fatalf("benign remote final signal=%v", signal)
	}
	if runtime.calls.Load() != 0 {
		t.Fatal("benign remote final ended the relay session")
	}
}

func TestReceiverPeerMonitorDoesNotSilenceRuntimeTermination(t *testing.T) {
	var stderr bytes.Buffer
	attempt := newCLIReceiverPeerAttempt()
	runtime := &cliReceiverRuntimeCloser{}
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition:   receiverPeerSessionUnavailable,
		retainedCause: sessionruntime.ErrRuntimeClosed,
	})
	var signal receiverPeerSignal

	(&App{Stderr: &stderr}).monitorReceiverPeer(attempt, runtime, func(observed receiverPeerSignal) {
		signal = observed
	})

	if signal != receiverPeerRuntimeTerminal || runtime.calls.Load() != 0 {
		t.Fatalf("runtime terminal signal=%v close_calls=%d", signal, runtime.calls.Load())
	}
	if !strings.Contains(stderr.String(), "authenticated runtime ended") {
		t.Fatalf("runtime terminal diagnostic=%q", stderr.String())
	}
}

func TestReceiverPeerMonitorKeepsUnexpectedAuthenticatedKindOperationLocal(t *testing.T) {
	var stderr bytes.Buffer
	attempt := newCLIReceiverPeerAttempt()
	runtime := &cliReceiverRuntimeCloser{}
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition:   receiverPeerFallbackAllowed,
		retainedCause: protocolsession.ErrUnknownMessageKind,
	})
	var signal receiverPeerSignal

	(&App{Stderr: &stderr}).monitorReceiverPeer(attempt, runtime, func(observed receiverPeerSignal) {
		signal = observed
	})

	if signal != receiverPeerFailed || runtime.calls.Load() != 0 {
		t.Fatalf("unexpected authenticated kind signal=%v close_calls=%d", signal, runtime.calls.Load())
	}
	if !strings.Contains(stderr.String(), "continuing through relay") {
		t.Fatalf("unexpected authenticated kind diagnostic=%q", stderr.String())
	}
}

func TestReceiverPeerStartsBeforeBlockingSelectionPlanning(t *testing.T) {
	peerStarted := make(chan struct{})
	selectionEntered := make(chan struct{})
	releaseSelection := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = beginReceiverPlanning(
			func() *activeReceiverPeer {
				close(peerStarted)
				return nil
			},
			func() (transfer.SelectionRules, error) {
				close(selectionEntered)
				<-releaseSelection
				return transfer.SelectionRules{}, nil
			},
		)
	}()
	select {
	case <-selectionEntered:
	case <-time.After(time.Second):
		t.Fatal("selection planning did not begin")
	}
	select {
	case <-peerStarted:
	default:
		t.Fatal("blocking selection planning began before the peer race")
	}
	close(releaseSelection)
	<-done
}
