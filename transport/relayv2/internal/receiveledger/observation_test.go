package receiveledger

import (
	"testing"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type eventRecorder struct{ events []Event }

func (r *eventRecorder) ObserveReceive(event Event) { r.events = append(r.events, event) }

func TestObservationsIdentifyRetainedDebtAndRefusedTransitions(t *testing.T) {
	var recorder eventRecorder
	config := testConfig()
	config.ConnectionID = 42
	config.Observer = &recorder
	ledger := testLedger(t, config)
	session := openSession(t, ledger, 7)
	receipt := receiveFrame(t, ledger, session, 21)
	requireError(t, ledger.Discard(receipt), ErrRetirement)
	requireError(t, ledger.Retire(session), nil)
	requireError(t, ledger.Discard(receipt), nil)
	requireError(t, ledger.Notice(session, v2.SettlementNotice{
		RelaySessionID: testID(7), TotalFrames: 1, TotalEncodedBytes: 21,
	}), nil)
	ledger.TakeSettlement(session)
	requireError(t, ledger.Forget(session), nil)
	ledger.Close()
	if len(recorder.events) != 8 {
		t.Fatal("missing lifecycle evidence", recorder.events)
	}
	refusal := recorder.events[1]
	if refusal.ConnectionID != 42 || refusal.SessionID != testID(7) ||
		refusal.Action != ActionDiscard || refusal.Failure != ErrRetirement ||
		refusal.Usage.Held != (Counts{1, 21}) {
		t.Fatal("refusal lacks identity/accounting context", refusal)
	}
	discard := recorder.events[3]
	if discard.Usage.Held != (Counts{}) || discard.Usage.Outstanding != (Counts{1, 21}) ||
		discard.Progress.Discarded != (Counts{1, 21}) {
		t.Fatal("discard hid unsettled debt", discard)
	}
	settled := recorder.events[5]
	if settled.Action != ActionSettlement || settled.Usage.Outstanding != (Counts{}) ||
		settled.Progress.Phase != Settled {
		t.Fatal("settlement evidence", settled)
	}
}
