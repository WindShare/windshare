package receiveledger

import (
	"math"
	"testing"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestConsumptionReadinessIsIndependentOfDeclaredCapacity(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		capacity v2.ReceiveCapacity
		size     uint64
	}{
		{"default capacity", testConfig().Capacity, 21},
		{"one frame", v2.ReceiveCapacity{Frames: 1, EncodedBytes: 1 << 20}, 21},
		{"few frames", v2.ReceiveCapacity{Frames: 3, EncodedBytes: 1 << 20}, 21},
		{"byte bound", v2.ReceiveCapacity{Frames: 256, EncodedBytes: maximumEnvelopeBytes}, maximumEnvelopeBytes},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			config := testConfig()
			config.Capacity = scenario.capacity
			ledger := testLedger(t, config)
			session := openSession(t, ledger, 1)
			for round := uint64(1); round <= 3; round++ {
				requireError(t, ledger.Consume(receiveFrame(t, ledger, session, scenario.size)), nil)
				if !ledger.FeedbackReady() {
					t.Fatal("declared capacity delayed a consumed frame")
				}
				feedback, ok := ledger.TakeConsumed()
				if !ok || feedback.TotalFrames != round || feedback.TotalEncodedBytes != scenario.size*round {
					t.Fatal("counters reset across batches", feedback, ok)
				}
			}
		})
	}
}

func TestWriterCoalescesConsumptionAcrossSessions(t *testing.T) {
	config := testConfig()
	config.Capacity = v2.ReceiveCapacity{Frames: 8, EncodedBytes: 1 << 20}
	ledger := testLedger(t, config)
	first := openSession(t, ledger, 1)
	second := openSession(t, ledger, 2)
	requireError(t, ledger.Consume(receiveFrame(t, ledger, first, 21)), nil)
	requireError(t, ledger.Consume(receiveFrame(t, ledger, first, 23)), nil)
	requireError(t, ledger.Consume(receiveFrame(t, ledger, second, 22)), nil)
	if !ledger.FeedbackReady() {
		t.Fatal("pending consumption was not eligible")
	}
	for _, id := range []uint64{1, 2} {
		feedback, ok := ledger.TakeConsumed()
		if !ok || feedback.RelaySessionID != testID(id) {
			t.Fatal("pending session lost its feedback", feedback, ok)
		}
		if id == 1 && (feedback.TotalFrames != 2 || feedback.TotalEncodedBytes != 44) {
			t.Fatal("writer did not coalesce the consumed prefix", feedback)
		}
	}
	if ledger.FeedbackReady() {
		t.Fatal("empty batch remained ready")
	}
	requireUsage(t, ledger, Usage{RetainedSessions: 2})
}

func TestBusySessionDoesNotStarveSiblingFeedback(t *testing.T) {
	config := testConfig()
	ledger := testLedger(t, config)
	first := openSession(t, ledger, 1)
	second := openSession(t, ledger, 2)
	requireError(t, ledger.Consume(receiveFrame(t, ledger, first, 21)), nil)
	requireError(t, ledger.Consume(receiveFrame(t, ledger, second, 21)), nil)
	ledger.TakeConsumed()
	requireError(t, ledger.Consume(receiveFrame(t, ledger, first, 21)), nil)
	feedback, ok := ledger.TakeConsumed()
	if !ok || feedback.RelaySessionID != testID(2) {
		t.Fatal("busy first session starved sibling", feedback, ok)
	}
	feedback, ok = ledger.TakeConsumed()
	if !ok || feedback.RelaySessionID != testID(1) || feedback.TotalFrames != 2 {
		t.Fatal("new consumption lost", feedback, ok)
	}
}

func TestSparseConsumptionNeedsNoAdditionalInput(t *testing.T) {
	ledger := testLedger(t, testConfig())
	session := openSession(t, ledger, 1)
	if ledger.FeedbackReady() {
		t.Fatal("empty ledger generated feedback")
	}
	requireError(t, ledger.Consume(receiveFrame(t, ledger, session, 23)), nil)
	feedback, ok := ledger.TakeConsumed()
	if !ok || feedback.TotalFrames != 1 || feedback.TotalEncodedBytes != 23 {
		t.Fatal("writer could not release sparse consumption", feedback, ok)
	}
}

func TestCumulativeCountersReachUint64BoundaryWithoutWrapping(t *testing.T) {
	ledger := testLedger(t, testConfig())
	session := openSession(t, ledger, 1)
	// A long, already-reported history can be seeded without billions of frames.
	history := Counts{math.MaxUint64/21 - 2, math.MaxUint64 - 42}
	f := session.flow
	f.Received, f.completed, f.reported = history, history, history
	f.Consumed, f.ConsumedPrefix = history, history
	for range 2 {
		requireError(t, ledger.Consume(receiveFrame(t, ledger, session, 21)), nil)
	}
	feedback, ok := ledger.TakeConsumed()
	if !ok || feedback.TotalFrames != history.Frames+2 || feedback.TotalEncodedBytes != math.MaxUint64 {
		t.Fatal("cumulative count truncated", feedback, ok)
	}
	wire, err := feedback.MarshalBinary()
	requireError(t, err, nil)
	parsed, err := v2.ParseDeliveryConsumed(wire)
	requireError(t, err, nil)
	if parsed != feedback {
		t.Fatal(parsed)
	}
	before := ledger.Usage()
	_, err = ledger.Receive(session, 21)
	requireError(t, err, ErrCounterExhausted)
	requireUsage(t, ledger, before)
	requireError(t, ledger.Retire(session), nil)
	requireError(t, ledger.Notice(session, v2.SettlementNotice(feedback)), nil)
	ack, ok := ledger.TakeSettlement(session)
	if !ok || ack != v2.SettlementAck(feedback) {
		t.Fatal("exhausted flow could not settle", ack, ok)
	}
}

func TestFrameCounterExhaustionIsAtomic(t *testing.T) {
	ledger := testLedger(t, testConfig())
	session := openSession(t, ledger, 1)
	session.flow.Received.Frames = math.MaxUint64
	_, err := ledger.Receive(session, 21)
	requireError(t, err, ErrCounterExhausted)
	requireUsage(t, ledger, Usage{RetainedSessions: 1})
}
