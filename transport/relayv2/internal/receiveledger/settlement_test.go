package receiveledger

import (
	"testing"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestNoticeWaitsForActualConsumptionOrDiscard(t *testing.T) {
	ledger := testLedger(t, testConfig())
	session := openSession(t, ledger, 1)
	first := receiveFrame(t, ledger, session, 21)
	second := receiveFrame(t, ledger, session, 22)
	third := receiveFrame(t, ledger, session, 23)
	requireError(t, ledger.Consume(first), nil)
	ledger.FlushConsumption()
	feedback, ok := ledger.TakeConsumed()
	if !ok || feedback.TotalEncodedBytes != 21 {
		t.Fatal(feedback, ok)
	}
	notice := v2.SettlementNotice{RelaySessionID: testID(1), TotalFrames: 3, TotalEncodedBytes: 66}
	requireError(t, ledger.Notice(session, notice), nil)
	if _, ok := ledger.TakeSettlement(session); ok {
		t.Fatal("notice acknowledged queued application work")
	}
	_, err := ledger.Receive(session, 21)
	requireError(t, err, ErrFrozenPrefix)
	requireError(t, ledger.Forget(session), ErrSettlementPending)
	requireError(t, ledger.Discard(second), nil)
	if _, ok := ledger.TakeSettlement(session); ok {
		t.Fatal("partial discard acknowledged whole prefix")
	}
	requireError(t, ledger.Consume(third), nil)
	progress, _ := ledger.Progress(session)
	if progress != (Progress{
		Phase: Draining, Received: Counts{3, 66}, Consumed: Counts{2, 44},
		Discarded: Counts{1, 22}, ConsumedPrefix: Counts{1, 21},
	}) {
		t.Fatal("consumption crossed a discarded hole", progress)
	}
	ledger.FlushConsumption()
	if _, ok := ledger.TakeConsumed(); ok {
		t.Fatal("WS2B claimed discarded data")
	}
	requireUsage(t, ledger, Usage{Outstanding: Counts{2, 45}, RetainedSessions: 1})
	ack, ok := ledger.TakeSettlement(session)
	if !ok || ack != v2.SettlementAck(notice) {
		t.Fatal("wrong final settlement", ack, ok)
	}
	wire, err := ack.MarshalBinary()
	requireError(t, err, nil)
	parsed, err := v2.ParseSettlementAck(wire)
	requireError(t, err, nil)
	if parsed != ack {
		t.Fatal("settlement wire mismatch", parsed)
	}
	requireUsage(t, ledger, Usage{RetainedSessions: 1})
	requireError(t, ledger.Forget(session), nil)
	requireUsage(t, ledger, Usage{})
}

func TestRetirementRetainsLateFramesAndDebtUntilNotice(t *testing.T) {
	config := testConfig()
	config.Capacity = v2.ReceiveCapacity{Frames: 2, EncodedBytes: 100}
	ledger := testLedger(t, config)
	retired := openSession(t, ledger, 1)
	sibling := openSession(t, ledger, 2)
	first := receiveFrame(t, ledger, retired, 21)
	requireError(t, ledger.Retire(retired), nil)
	requireError(t, ledger.Retire(retired), nil)
	requireError(t, ledger.Discard(first), nil)
	late := receiveFrame(t, ledger, retired, 22)
	requireError(t, ledger.Discard(late), nil)
	requireUsage(t, ledger, Usage{Outstanding: Counts{2, 43}, RetainedSessions: 2})
	if _, ok := ledger.TakeSettlement(retired); ok {
		t.Fatal("local retirement replaced WS2N")
	}
	_, err := ledger.Receive(sibling, 21)
	requireError(t, err, ErrCapacity)
	requireError(t, ledger.Notice(retired, v2.SettlementNotice{
		RelaySessionID: testID(1), TotalFrames: 2, TotalEncodedBytes: 43,
	}), nil)
	if _, ok := ledger.TakeSettlement(retired); !ok {
		t.Fatal("discarded prefix did not settle")
	}
	requireError(t, ledger.Forget(retired), nil)
	requireError(t, ledger.Consume(receiveFrame(t, ledger, sibling, 23)), nil)
	feedback, ok := ledger.TakeConsumed()
	if !ok || feedback.RelaySessionID != testID(2) {
		t.Fatal("healthy sibling could not continue", feedback, ok)
	}
}

func TestMismatchedNoticesCannotRetireOrRefundAFlow(t *testing.T) {
	ledger := testLedger(t, testConfig())
	session := openSession(t, ledger, 1)
	receiveFrame(t, ledger, session, 21)
	for _, notice := range []v2.SettlementNotice{
		{RelaySessionID: testID(2), TotalFrames: 1, TotalEncodedBytes: 21},
		{RelaySessionID: testID(1)},
		{RelaySessionID: testID(1), TotalFrames: 2, TotalEncodedBytes: 42},
		{RelaySessionID: testID(1), TotalFrames: 1, TotalEncodedBytes: 22},
		{RelaySessionID: testID(1), TotalFrames: 0, TotalEncodedBytes: 21},
	} {
		before := ledger.Usage()
		requireError(t, ledger.Notice(session, notice), ErrSettlement)
		requireUsage(t, ledger, before)
		if progress, _ := ledger.Progress(session); progress.Phase != Active {
			t.Fatal("invalid notice mutated retirement", progress)
		}
	}
}

func TestConsumedPrefixDoesNotAcknowledgeRetirement(t *testing.T) {
	for _, flush := range []bool{false, true} {
		ledger := testLedger(t, testConfig())
		session := openSession(t, ledger, 1)
		requireError(t, ledger.Consume(receiveFrame(t, ledger, session, 21)), nil)
		if flush {
			ledger.FlushConsumption()
			ledger.TakeConsumed()
		}
		requireError(t, ledger.Retire(session), nil)
		if _, ok := ledger.TakeSettlement(session); ok {
			t.Fatal("consumption acknowledged an unseen notice")
		}
		requireError(t, ledger.Forget(session), ErrSettlementPending)
		requireError(t, ledger.Notice(session, v2.SettlementNotice{
			RelaySessionID: testID(1), TotalFrames: 1, TotalEncodedBytes: 21,
		}), nil)
		if _, ok := ledger.TakeSettlement(session); !ok {
			t.Fatal("fully consumed flow did not settle")
		}
		ledger.FlushConsumption()
		if _, ok := ledger.TakeConsumed(); ok {
			t.Fatal("settlement left duplicate consumption pending")
		}
		requireUsage(t, ledger, Usage{RetainedSessions: 1})
	}
}

func TestSettlementReplayAndStaleCallbacksCannotTouchSiblings(t *testing.T) {
	ledger := testLedger(t, testConfig())
	old := openSession(t, ledger, 1)
	sibling := openSession(t, ledger, 2)
	oldFrame := receiveFrame(t, ledger, old, 21)
	siblingFrame := receiveFrame(t, ledger, sibling, 25)
	notice := v2.SettlementNotice{RelaySessionID: testID(1), TotalFrames: 1, TotalEncodedBytes: 21}
	requireError(t, ledger.Notice(old, notice), nil)
	requireError(t, ledger.Discard(oldFrame), nil)
	firstAck, ok := ledger.TakeSettlement(old)
	if !ok {
		t.Fatal("missing first acknowledgement")
	}
	if _, ok := ledger.TakeSettlement(old); ok {
		t.Fatal("acknowledgement spun without a repeated notice")
	}
	before := ledger.Usage()
	requireError(t, ledger.Notice(old, notice), nil)
	requireError(t, ledger.Forget(old), ErrSettlementPending)
	secondAck, ok := ledger.TakeSettlement(old)
	if !ok || secondAck != firstAck {
		t.Fatal("notice replay changed acknowledgement")
	}
	requireUsage(t, ledger, before)
	requireError(t, ledger.Forget(old), nil)
	replacement := openSession(t, ledger, 3)
	receiveFrame(t, ledger, replacement, 26)
	before = ledger.Usage()
	requireError(t, ledger.Consume(oldFrame), ErrSession)
	requireError(t, ledger.Discard(oldFrame), ErrSession)
	requireError(t, ledger.Retire(old), ErrSession)
	requireError(t, ledger.Notice(old, notice), ErrSession)
	requireError(t, ledger.Forget(old), ErrSession)
	_, err := ledger.Receive(old, 21)
	requireError(t, err, ErrSession)
	if _, ok := ledger.TakeSettlement(old); ok {
		t.Fatal("forgotten flow was recreated")
	}
	requireUsage(t, ledger, before)
	requireError(t, ledger.Consume(siblingFrame), nil)
	ledger.FlushConsumption()
	feedback, ok := ledger.TakeConsumed()
	if !ok || feedback.RelaySessionID != testID(2) || feedback.TotalEncodedBytes != 25 {
		t.Fatal("stale callbacks affected sibling progress", feedback, ok)
	}
}

func TestReplacementConnectionRejectsOldTokensWithSameWireIdentity(t *testing.T) {
	oldLedger := testLedger(t, testConfig())
	old := openSession(t, oldLedger, 1)
	receipt := receiveFrame(t, oldLedger, old, 21)
	oldLedger.Close()

	config := testConfig()
	config.ConnectionID++
	replacement := testLedger(t, config)
	current := openSession(t, replacement, 1)
	receiveFrame(t, replacement, current, 21)
	before := replacement.Usage()
	requireError(t, replacement.Consume(receipt), ErrSession)
	requireError(t, replacement.Notice(old, v2.SettlementNotice{
		RelaySessionID: testID(1), TotalFrames: 1, TotalEncodedBytes: 21,
	}), ErrSession)
	requireUsage(t, replacement, before)
}

func TestClosedConnectionCannotProduceFeedbackOrReopen(t *testing.T) {
	ledger := testLedger(t, testConfig())
	session := openSession(t, ledger, 1)
	first := receiveFrame(t, ledger, session, 21)
	second := receiveFrame(t, ledger, session, 22)
	requireError(t, ledger.Consume(first), nil)
	requireError(t, ledger.Notice(session, v2.SettlementNotice{
		RelaySessionID: testID(1), TotalFrames: 2, TotalEncodedBytes: 43,
	}), nil)
	before := ledger.Usage()
	ledger.Close()
	ledger.Close()
	ledger.FlushConsumption()
	if _, ok := ledger.TakeConsumed(); ok {
		t.Fatal("closed connection produced consumption")
	}
	if _, ok := ledger.TakeSettlement(session); ok {
		t.Fatal("closed connection produced settlement")
	}
	requireError(t, ledger.Consume(second), ErrClosed)
	requireError(t, ledger.Discard(second), ErrClosed)
	requireError(t, ledger.Retire(session), ErrClosed)
	requireError(t, ledger.Notice(session, v2.SettlementNotice{}), ErrClosed)
	requireError(t, ledger.Forget(session), ErrClosed)
	_, err := ledger.Receive(session, 21)
	requireError(t, err, ErrClosed)
	_, err = ledger.Open(testID(2))
	requireError(t, err, ErrClosed)
	if _, ok := ledger.Progress(session); ok {
		t.Fatal("closed generation remained accessible")
	}
	requireUsage(t, ledger, before)
}

func TestUnknownEventsDoNotAllocateSessions(t *testing.T) {
	ledger := testLedger(t, testConfig())
	requireError(t, ledger.Consume(Receipt{}), ErrSession)
	requireError(t, ledger.Notice(Session{}, v2.SettlementNotice{RelaySessionID: testID(1)}), ErrSession)
	requireUsage(t, ledger, Usage{})
}
