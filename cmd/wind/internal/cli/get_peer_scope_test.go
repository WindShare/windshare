package cli

import (
	"strings"
	"testing"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/session/protocolsession"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func TestMonitorReceiverPeerUnsafeDispositionRevokesQueuedAdmissionWithoutFallback(t *testing.T) {
	relay := newFakeReceiverContentSuspension()
	claimGate := make(chan struct{})
	admission, err := newRelayContentAdmissionWithExecution(
		relay,
		receiverAdmissionExecution{claimGate: claimGate},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admission.Close()
		admission.Wait()
	})

	if err := admission.AdmitRelayOnly(); err != nil {
		t.Fatal(err)
	}
	workerDone := admission.decisionWorkerDone()
	if workerDone == nil {
		t.Fatal("queued admission has no owned worker")
	}

	sessionFault, err := transferfault.NewSession(
		transferfault.ScopeSessionTerminal, transferfault.SessionProtocol,
	)
	if err != nil {
		t.Fatal(err)
	}
	sessionFailure := transferfault.Wrap(
		sessionFault,
		protocolsession.ErrInvalidOperationFailure,
	)
	classes := v2peer.ReceiverCauseClasses(sessionFailure)
	if len(classes) == 0 {
		t.Fatal("exact closed session fault produced no diagnostic classes")
	}
	attempt := newCLIReceiverPeerAttempt()
	// The CLI seam begins after v2peer's sealed validation boundary: the typed
	// disposition supplies authority, while the closed session fault remains diagnostic only.
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition:   receiverPeerSessionUnsafe,
		retainedCause: sessionFailure,
	})

	commandRuntime, stderr := newGetReportingRuntime(t, false, false)
	receiverRuntime := &cliReceiverRuntimeCloser{}
	var signals []receiverPeerSignal
	var observeErr error
	(&App{}).monitorReceiverPeer(
		attempt, receiverRuntime, protocolsession.ProtocolSessionID{1},
		getObservation{runtime: commandRuntime}, func(signal receiverPeerSignal) {
			signals = append(signals, signal)
			if err := admission.ObservePeer(signal); err != nil {
				observeErr = err
			}
		},
	)
	commandRuntime.Close()

	if observeErr != nil {
		t.Fatalf("apply peer signal: %v", observeErr)
	}
	if len(signals) != 1 || signals[0] != receiverPeerSessionFatal {
		t.Fatalf("peer signals=%v, want exactly one session-fatal signal", signals)
	}
	for _, signal := range signals {
		if signal == receiverPeerFailed || signal == receiverPeerDetached {
			t.Fatalf("unsafe disposition emitted fallback signal=%v", signal)
		}
	}
	if calls := receiverRuntime.calls.Load(); calls != 1 {
		t.Fatalf("fatal monitor branch Close calls=%d, want 1", calls)
	}
	if err := admission.AdmitRelayOnly(); err != nil {
		t.Fatalf("closed admission accepted follow-up selection with error: %v", err)
	}

	admission.Wait()
	if _, ok := <-admission.Decision(); ok {
		t.Fatal("unsafe disposition published a relay-admission decision")
	}
	<-workerDone
	if resumed := relay.count(); resumed != 0 {
		t.Fatalf("unsafe disposition resumed relay %d time(s)", resumed)
	}

	diagnostic := stderr.String()
	if !strings.Contains(diagnostic, "The transfer session failed.") {
		t.Fatalf("unsafe-disposition diagnostic=%q", diagnostic)
	}
	if strings.Contains(diagnostic, "continuing") {
		t.Fatalf("unsafe-disposition diagnostic advertised fallback=%q", diagnostic)
	}
}
