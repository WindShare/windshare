package receive

import (
	"testing"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/session/protocolsession"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func TestMonitorReceiverPeerUnsafeDispositionRevokesQueuedAdmissionWithoutFallback(t *testing.T) {
	relay := newFakeReceiverContentSuspension()
	claimGate := make(chan struct{})
	plan, err := ConnectivityAuto.receiverPlan()
	if err != nil {
		t.Fatal(err)
	}
	decided := false
	admission, err := newReceiverContentAdmission(
		plan,
		relay,
		receiverAdmissionHooks{
			claimGate:  claimGate,
			onDecision: func(receiverAdmissionDecision) { decided = true },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admission.Close()
		admission.Wait()
	})

	if err := admission.AdmitRelay(); err != nil {
		t.Fatal(err)
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

	observation, events := newTestObservation(t)
	receiverRuntime := &testRuntimeCloser{}
	var signals []receiverPeerSignal
	var observeErr error
	(&runner{}).monitorReceiverPeer(
		attempt, receiverRuntime, protocolsession.ProtocolSessionID{1},
		observation, func(signal receiverPeerSignal) {
			signals = append(signals, signal)
			if err := admission.ObservePeer(signal); err != nil {
				observeErr = err
			}
		},
	)

	if observeErr != nil {
		t.Fatalf("apply peer signal: %v", observeErr)
	}
	if len(signals) != 1 || signals[0] != receiverPeerSessionFatal {
		t.Fatalf("peer signals=%v, want exactly one session-fatal signal", signals)
	}
	if calls := receiverRuntime.calls.Load(); calls != 1 {
		t.Fatalf("fatal monitor branch Close calls=%d, want 1", calls)
	}
	close(claimGate)
	if err := admission.AdmitRelay(); err != nil {
		t.Fatalf("closed admission returned error: %v", err)
	}
	admission.Wait()
	if decided {
		t.Fatal("unsafe disposition published a relay-admission decision")
	}
	if resumed := relay.count(); resumed != 0 {
		t.Fatalf("unsafe disposition resumed relay %d time(s)", resumed)
	}

	if len(*events) != 1 {
		t.Fatalf("diagnostics=%v", *events)
	}
	if warning, ok := (*events)[0].(Warning); !ok || warning.Cause != sessionFailure {
		t.Fatalf("diagnostic=%v", (*events)[0])
	}
}
