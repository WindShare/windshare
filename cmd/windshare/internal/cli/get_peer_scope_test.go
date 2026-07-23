package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

func TestMonitorReceiverPeerUnsafeDispositionRevokesQueuedAdmissionWithoutFallback(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	claimGate := make(chan struct{})
	admission, err := newRelayContentAdmissionWithExecution(
		downloadT0,
		clock,
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

	if err := admission.ObserveSelection(transfer.SelectionSmall); err != nil {
		t.Fatal(err)
	}
	workerDone := admission.decisionWorkerDone()
	if workerDone == nil {
		t.Fatal("queued admission has no owned worker")
	}

	sessionFailure := transfer.NewSessionFailure(protocolsession.ErrInvalidOperationFailure)
	classes := v2peer.ReceiverCauseClasses(sessionFailure)
	if len(classes) == 0 {
		t.Fatal("exact core SessionFailure produced no diagnostic classes")
	}
	attempt := newCLIReceiverPeerAttempt()
	// The CLI seam begins after v2peer's sealed validation boundary: the typed
	// disposition supplies authority, while SessionFailure remains diagnostic only.
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition:   receiverPeerSessionUnsafe,
		retainedCause: sessionFailure,
	})

	var stderr bytes.Buffer
	app := &App{Stderr: &stderr}
	runtime := &cliReceiverRuntimeCloser{}
	var signals []receiverPeerSignal
	var observeErr error
	app.monitorReceiverPeer(attempt, runtime, func(signal receiverPeerSignal) {
		signals = append(signals, signal)
		if err := admission.ObservePeer(signal); err != nil {
			observeErr = err
		}
	})

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
	if calls := runtime.calls.Load(); calls != 1 {
		t.Fatalf("fatal monitor branch Close calls=%d, want 1", calls)
	}
	if err := admission.ObserveSelection(transfer.SelectionSmall); err != nil {
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
	if !strings.Contains(diagnostic, "closing the session") {
		t.Fatalf("unsafe-disposition diagnostic=%q", diagnostic)
	}
	if strings.Contains(diagnostic, "continuing") {
		t.Fatalf("unsafe-disposition diagnostic advertised fallback=%q", diagnostic)
	}
}
