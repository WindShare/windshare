package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
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

func TestReceiverContentPathsAddsRelayWithoutInventingPeerTimeout(t *testing.T) {
	runtime, stderr := newGetReportingRuntime(t, true, false)
	paths := newReceiverContentPaths(getObservation{runtime: runtime})

	paths.observePeer(receiverPeerReady)
	(&App{}).observeRelayContentAdmission(receiverAdmissionTriggerDeadline, paths)
	runtime.Close()

	diagnostic := stderr.String()
	direct := strings.Index(diagnostic, "Content path: Direct")
	combined := strings.Index(diagnostic, "Content path: Direct + Relay")
	if direct < 0 || combined <= direct {
		t.Fatalf("content path transitions=%q", diagnostic)
	}
	if strings.Contains(diagnostic, "Warning:") || strings.Contains(diagnostic, "unavailable") {
		t.Fatalf("relay policy deadline was rendered as a peer failure: %q", diagnostic)
	}
}

func TestReceiverContentPathsReportsRealDirectDetachAfterRelayAdmission(t *testing.T) {
	runtime, stderr := newGetReportingRuntime(t, true, false)
	paths := newReceiverContentPaths(getObservation{runtime: runtime})

	paths.observePeer(receiverPeerReady)
	paths.relayAdmitted(receiverAdmissionTriggerDeadline)
	paths.observePeer(receiverPeerDetached)
	runtime.Close()

	diagnostic := stderr.String()
	if !strings.Contains(diagnostic, "Direct path unavailable; using Relay.") ||
		!strings.Contains(diagnostic, "Content path: Relay") {
		t.Fatalf("direct detach transitions=%q", diagnostic)
	}
}

type cliReceiverRuntimeCloser struct{ calls atomic.Int32 }

func (runtime *cliReceiverRuntimeCloser) Close() { runtime.calls.Add(1) }

func TestReceiverPeerSetupFailureLogsSafePhaseAndCauseClass(t *testing.T) {
	runtime, stderr := newGetReportingRuntime(t, false, false)
	app := &App{
		receiverPeerFactory: func() (receiverPeerStarter, error) {
			return nil, v2peer.ErrNegotiation
		},
	}
	var signal receiverPeerSignal
	peer := app.startReceiverPeer(context.Background(), nil, getObservation{runtime: runtime}, func(observed receiverPeerSignal) {
		signal = observed
	})
	runtime.Close()
	if peer != nil || signal != receiverPeerFailed {
		t.Fatalf("setup failure peer=%v signal=%v", peer, signal)
	}
	if diagnostic := stderr.String(); !strings.Contains(diagnostic, "The direct connection is unavailable.") ||
		strings.Contains(diagnostic, "factory") || strings.Contains(diagnostic, v2peer.ErrNegotiation.Error()) {
		t.Fatalf("setup failure diagnostic=%q", diagnostic)
	}
}

func TestReceiverPeerSetupFailureDistinguishesEveryPhase(t *testing.T) {
	if got := receiverPeerSetupFailureCode(receiverPeerSetupFactory); got != clievent.FailurePeerConfiguration {
		t.Fatalf("factory failure code=%v", got)
	}
	if got := receiverPeerSetupFailureCode(receiverPeerSetupSignaling); got != clievent.FailurePeerSignaling {
		t.Fatalf("signaling failure code=%v", got)
	}
	if got := receiverPeerSetupFailureCode(receiverPeerSetupStart); got != clievent.FailurePeerNegotiation {
		t.Fatalf("start failure code=%v", got)
	}
}

func TestReceiverPeerTerminationTraceIsDrainedSynchronously(t *testing.T) {
	runtime, stderr := newGetReportingRuntime(t, false, false)
	traces := make(chan receiverPeerTerminationTrace, 1)
	traces <- receiverPeerTerminationTrace{
		diagnosticsTruncated: true,
		retainedCauseClasses: []v2peer.ReceiverCauseClass{v2peer.ReceiverCauseProtocol},
		channelDrainFailed:   true,
	}

	(&App{}).awaitReceiverTerminationTrace(traces, getObservation{runtime: runtime}, false)
	runtime.Close()

	diagnostic := stderr.String()
	if !strings.Contains(diagnostic, "The direct connection is unavailable.") ||
		strings.Contains(diagnostic, "diagnostics_truncated") || strings.Contains(diagnostic, "protocol") {
		t.Fatalf("termination trace diagnostic=%q", diagnostic)
	}
}

func TestReceiverPeerMonitorClosesSessionForAuthenticatedAuthorityViolation(t *testing.T) {
	commandRuntime, _ := newGetReportingRuntime(t, false, false)
	attempt := newCLIReceiverPeerAttempt()
	receiverRuntime := &cliReceiverRuntimeCloser{}
	fatalCause := errors.New("binding substitution")
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition:   receiverPeerSessionUnsafe,
		retainedCause: fatalCause,
	})
	var signal receiverPeerSignal

	(&App{}).monitorReceiverPeer(
		attempt, receiverRuntime, protocolsession.ProtocolSessionID{1},
		getObservation{runtime: commandRuntime}, func(observed receiverPeerSignal) { signal = observed },
	)
	commandRuntime.Close()

	if receiverRuntime.calls.Load() != 1 {
		t.Fatalf("runtime close calls = %d", receiverRuntime.calls.Load())
	}
	if signal != receiverPeerSessionFatal {
		t.Fatalf("fatal signal=%v", signal)
	}
}

func TestReceiverPeerMonitorReportsAttemptLocalFailureWithoutOwningFallback(t *testing.T) {
	attempt := newCLIReceiverPeerAttempt()
	receiverRuntime := &cliReceiverRuntimeCloser{}
	attempt.finish(errors.New("ICE negotiation failed"))
	var signal receiverPeerSignal

	(&App{}).monitorReceiverPeer(
		attempt, receiverRuntime, protocolsession.ProtocolSessionID{1}, getObservation{},
		func(observed receiverPeerSignal) { signal = observed },
	)

	if receiverRuntime.calls.Load() != 0 {
		t.Fatal("attempt-local peer failure closed the relay session")
	}
	if signal != receiverPeerFailed {
		t.Fatalf("fallback signal=%v", signal)
	}
}

func TestReceiverPeerMonitorRetainsJoinedCancellationFailure(t *testing.T) {
	attempt := newCLIReceiverPeerAttempt()
	receiverRuntime := &cliReceiverRuntimeCloser{}
	retained := errors.New("ICE teardown failed")
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition:   receiverPeerFallbackAllowed,
		retainedCause: retained,
	})
	var signal receiverPeerSignal

	(&App{}).monitorReceiverPeer(
		attempt, receiverRuntime, protocolsession.ProtocolSessionID{1}, getObservation{},
		func(observed receiverPeerSignal) { signal = observed },
	)

	if signal != receiverPeerFailed {
		t.Fatalf("joined cancellation residual signal=%v", signal)
	}
	if receiverRuntime.calls.Load() != 0 {
		t.Fatal("attempt-local residual closed the relay session")
	}
}

func TestReceiverPeerMonitorSignalsReadyThenCleanDetach(t *testing.T) {
	commandRuntime, _ := newGetReportingRuntime(t, false, false)
	attempt := newCLIReceiverPeerAttempt()
	attempt.lane = sessionruntime.LaneIdentity{ID: 3, Epoch: 1}
	receiverRuntime := &cliReceiverRuntimeCloser{}
	signals := make(chan receiverPeerSignal, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&App{}).monitorReceiverPeer(
			attempt, receiverRuntime, protocolsession.ProtocolSessionID{1},
			getObservation{runtime: commandRuntime}, func(signal receiverPeerSignal) { signals <- signal },
		)
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
	commandRuntime.Close()
	if receiverRuntime.calls.Load() != 0 {
		t.Fatal("clean peer detach closed the authenticated relay session")
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
		(&App{}).monitorReceiverPeer(
			attempt, runtime, protocolsession.ProtocolSessionID{1}, getObservation{},
			func(signal receiverPeerSignal) { signals <- signal },
		)
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

func TestReceiverPeerMonitorSuppressesLocalCloseCleanupResidue(t *testing.T) {
	commandRuntime, stderr := newGetReportingRuntime(t, false, false)
	attempt := newCLIReceiverPeerAttempt()
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition: receiverPeerLocalStop, retainedCause: errors.New("peer cleanup residue"),
	})
	runtime := &cliReceiverRuntimeCloser{}
	signals := make(chan receiverPeerSignal, 1)

	locallyCanceled := (&App{}).monitorReceiverPeer(
		attempt, runtime, protocolsession.ProtocolSessionID{1},
		getObservation{runtime: commandRuntime}, func(signal receiverPeerSignal) { signals <- signal },
	)
	traces := make(chan receiverPeerTerminationTrace, 1)
	traces <- receiverPeerTerminationTrace{
		retainedCauseClasses: []v2peer.ReceiverCauseClass{v2peer.ReceiverCauseChannelAdmission},
	}
	(&App{}).awaitReceiverTerminationTrace(
		traces, getObservation{runtime: commandRuntime}, locallyCanceled,
	)
	commandRuntime.Close()

	if !locallyCanceled || runtime.calls.Load() != 0 || stderr.Len() != 0 {
		t.Fatalf("local cleanup: canceled=%t close_calls=%d diagnostic=%q",
			locallyCanceled, runtime.calls.Load(), stderr.String())
	}
	select {
	case signal := <-signals:
		t.Fatalf("local cleanup emitted peer signal=%v", signal)
	default:
	}
}

func TestReceiverPeerMonitorTreatsBenignRemoteFinalAsPathFailure(t *testing.T) {
	attempt := newCLIReceiverPeerAttempt()
	runtime := &cliReceiverRuntimeCloser{}
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition: receiverPeerFallbackAllowed,
	})
	var signal receiverPeerSignal

	(&App{}).monitorReceiverPeer(
		attempt, runtime, protocolsession.ProtocolSessionID{1}, getObservation{},
		func(observed receiverPeerSignal) { signal = observed },
	)

	if signal != receiverPeerFailed {
		t.Fatalf("benign remote final signal=%v", signal)
	}
	if runtime.calls.Load() != 0 {
		t.Fatal("benign remote final ended the relay session")
	}
}

func TestReceiverPeerMonitorDoesNotSilenceRuntimeTermination(t *testing.T) {
	commandRuntime, stderr := newGetReportingRuntime(t, false, false)
	attempt := newCLIReceiverPeerAttempt()
	receiverRuntime := &cliReceiverRuntimeCloser{}
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition:   receiverPeerSessionUnavailable,
		retainedCause: sessionruntime.ErrRuntimeClosed,
	})
	var signal receiverPeerSignal

	(&App{}).monitorReceiverPeer(
		attempt, receiverRuntime, protocolsession.ProtocolSessionID{1},
		getObservation{runtime: commandRuntime}, func(observed receiverPeerSignal) { signal = observed },
	)
	commandRuntime.Close()

	if signal != receiverPeerRuntimeTerminal || receiverRuntime.calls.Load() != 0 {
		t.Fatalf("runtime terminal signal=%v close_calls=%d", signal, receiverRuntime.calls.Load())
	}
	if !strings.Contains(stderr.String(), "An unexpected error occurred.") {
		t.Fatalf("runtime terminal diagnostic=%q", stderr.String())
	}
}

func TestReceiverPeerMonitorKeepsUnexpectedAuthenticatedKindOperationLocal(t *testing.T) {
	attempt := newCLIReceiverPeerAttempt()
	receiverRuntime := &cliReceiverRuntimeCloser{}
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition:   receiverPeerFallbackAllowed,
		retainedCause: protocolsession.ErrUnknownMessageKind,
	})
	var signal receiverPeerSignal

	(&App{}).monitorReceiverPeer(
		attempt, receiverRuntime, protocolsession.ProtocolSessionID{1}, getObservation{},
		func(observed receiverPeerSignal) { signal = observed },
	)

	if signal != receiverPeerFailed || receiverRuntime.calls.Load() != 0 {
		t.Fatalf("unexpected authenticated kind signal=%v close_calls=%d", signal, receiverRuntime.calls.Load())
	}
}

func TestReceiverPeerStartsBeforeBlockingSelectionPlanning(t *testing.T) {
	plan, err := ConnectivityAuto.receiverPlan()
	if err != nil {
		t.Fatal(err)
	}
	peerStarted := make(chan struct{})
	selectionEntered := make(chan struct{})
	releaseSelection := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = beginReceiverPlanning(
			plan,
			func() *activeReceiverPeer {
				close(peerStarted)
				return nil
			},
			func() error {
				t.Error("auto planning resumed relay-only path")
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

func TestReceiverRelayOnlyPlanningNeverCreatesPeerAttempt(t *testing.T) {
	plan, err := ConnectivityRelayOnly.receiverPlan()
	if err != nil {
		t.Fatal(err)
	}
	peerStarts := 0
	relayResumes := 0
	_, _, err = beginReceiverPlanning(
		plan,
		func() *activeReceiverPeer {
			peerStarts++
			return nil
		},
		func() error {
			relayResumes++
			return nil
		},
		func() (transfer.SelectionRules, error) { return transfer.SelectionRules{}, nil },
	)
	if err != nil || peerStarts != 0 || relayResumes != 1 {
		t.Fatalf("relay-only planning: err=%v peer_starts=%d relay_resumes=%d", err, peerStarts, relayResumes)
	}
}

func TestReceiverP2POnlyPlanningRequiresPeerAndNeverAdmitsRelay(t *testing.T) {
	plan, err := ConnectivityP2POnly.receiverPlan()
	if err != nil {
		t.Fatal(err)
	}
	relayAdmissions := 0
	peer := &activeReceiverPeer{}
	got, _, err := beginReceiverPlanning(
		plan,
		func() *activeReceiverPeer { return peer },
		func() error {
			relayAdmissions++
			return nil
		},
		func() (transfer.SelectionRules, error) { return transfer.SelectionRules{}, nil },
	)
	if err != nil || got != peer || relayAdmissions != 0 {
		t.Fatalf("p2p-only planning: peer=%p err=%v relay_admissions=%d", got, err, relayAdmissions)
	}

	_, _, err = beginReceiverPlanning(
		plan,
		func() *activeReceiverPeer { return nil },
		func() error {
			t.Fatal("p2p-only planning admitted relay content")
			return nil
		},
		func() (transfer.SelectionRules, error) {
			t.Fatal("selection resolved without the required direct peer")
			return transfer.SelectionRules{}, nil
		},
	)
	if !errors.Is(err, errReceiverP2PPathUnavailable) {
		t.Fatalf("missing p2p-only peer error=%v", err)
	}
}

func TestConnectivityPolicyRejectsUnknownValues(t *testing.T) {
	for name, want := range map[string]ConnectivityPolicy{
		"auto": ConnectivityAuto, "relay-only": ConnectivityRelayOnly, "p2p-only": ConnectivityP2POnly,
	} {
		got, err := ParseConnectivityPolicy(name)
		if err != nil || got != want || got.String() != name {
			t.Fatalf("parse %q = %v, %v", name, got, err)
		}
	}
	if _, err := ParseConnectivityPolicy("relay"); !errors.Is(err, ErrInvalidConnectivityPolicy) {
		t.Fatalf("unknown policy error = %v", err)
	}
}
