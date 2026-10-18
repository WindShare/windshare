package receiveledger

import (
	"encoding/binary"
	"errors"
	"testing"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func testID(value uint64) (id v2.RelaySessionID) {
	binary.BigEndian.PutUint64(id[:], value)
	return id
}

func testConfig() Config {
	return Config{
		ConnectionID: 1,
		Capacity:     v2.ReceiveCapacity{Frames: 256, EncodedBytes: 16 << 20},
		MaxSessions:  8,
	}
}

func testLedger(t *testing.T, config Config) *Ledger {
	t.Helper()
	ledger, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func openSession(t *testing.T, ledger *Ledger, id uint64) Session {
	t.Helper()
	session, err := ledger.Open(testID(id))
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func receiveFrame(t *testing.T, ledger *Ledger, session Session, size uint64) Receipt {
	t.Helper()
	receipt, err := ledger.Receive(session, size)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func requireError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("error = %v, want %v", got, want)
	}
}

func requireUsage(t *testing.T, ledger *Ledger, want Usage) {
	t.Helper()
	if got := ledger.Usage(); got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestConnectionBudgetIsSharedAcrossSessions(t *testing.T) {
	config := testConfig()
	config.Capacity = v2.ReceiveCapacity{Frames: 2, EncodedBytes: 50}
	ledger := testLedger(t, config)
	first := openSession(t, ledger, 1)
	second := openSession(t, ledger, 2)
	a := receiveFrame(t, ledger, first, 21)
	b := receiveFrame(t, ledger, second, 29)
	full := Usage{Held: Counts{2, 50}, Outstanding: Counts{2, 50}, RetainedSessions: 2}
	requireUsage(t, ledger, full)
	if _, ok := ledger.TakeConsumed(); ok {
		t.Fatal("network arrival generated consumption")
	}
	_, err := ledger.Receive(second, 21)
	requireError(t, err, ErrCapacity)
	requireUsage(t, ledger, full)

	requireError(t, ledger.Consume(a), nil)
	_, err = ledger.Receive(second, 21)
	requireError(t, err, ErrCapacity)
	requireUsage(t, ledger, Usage{
		Held: Counts{1, 29}, Outstanding: Counts{2, 50}, PendingConsumed: Counts{1, 21}, RetainedSessions: 2,
	})
	feedback, ok := ledger.TakeConsumed()
	if !ok || feedback != (v2.DeliveryConsumed{RelaySessionID: testID(1), TotalFrames: 1, TotalEncodedBytes: 21}) {
		t.Fatal("full envelope feedback", feedback, ok)
	}
	wire, err := feedback.MarshalBinary()
	requireError(t, err, nil)
	decoded, err := v2.ParseDeliveryConsumed(wire)
	requireError(t, err, nil)
	if len(wire) != v2.DeliveryConsumedBytes || decoded != feedback {
		t.Fatal("cumulative wire control mismatch")
	}
	_, err = ledger.Receive(first, 22)
	requireError(t, err, ErrCapacity)
	receiveFrame(t, ledger, first, 21)
	requireError(t, ledger.Consume(b), nil)
	feedback, ok = ledger.TakeConsumed()
	if !ok || feedback.RelaySessionID != testID(2) || feedback.TotalEncodedBytes != 29 {
		t.Fatal("sibling consumption lost", feedback, ok)
	}
	requireUsage(t, ledger, Usage{Held: Counts{1, 21}, Outstanding: Counts{1, 21}, RetainedSessions: 2})
}

func TestFrameAndByteLimitsAreIndependent(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		capacity   v2.ReceiveCapacity
		secondSize uint64
	}{
		{"frames", v2.ReceiveCapacity{Frames: 1, EncodedBytes: 1 << 20}, 21},
		{"bytes", v2.ReceiveCapacity{Frames: 100, EncodedBytes: 42}, 22},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			config := testConfig()
			config.Capacity = scenario.capacity
			ledger := testLedger(t, config)
			session := openSession(t, ledger, 1)
			receiveFrame(t, ledger, session, 21)
			before := ledger.Usage()
			_, err := ledger.Receive(session, scenario.secondSize)
			requireError(t, err, ErrCapacity)
			requireUsage(t, ledger, before)
		})
	}
}

func TestReceiptsRequireFIFOAndCannotRefundTwice(t *testing.T) {
	ledger := testLedger(t, testConfig())
	session := openSession(t, ledger, 1)
	first := receiveFrame(t, ledger, session, 21)
	second := receiveFrame(t, ledger, session, 40)
	before := ledger.Usage()
	requireError(t, ledger.Consume(second), ErrReceipt)
	requireError(t, ledger.Discard(first), ErrRetirement)
	requireUsage(t, ledger, before)
	requireError(t, ledger.Consume(first), nil)
	after := ledger.Usage()
	requireError(t, ledger.Consume(first), ErrReceipt)
	requireError(t, ledger.Discard(first), ErrReceipt)
	requireUsage(t, ledger, after)
	requireError(t, ledger.Consume(second), nil)
	feedback, ok := ledger.TakeConsumed()
	if !ok || feedback.TotalFrames != 2 || feedback.TotalEncodedBytes != 61 {
		t.Fatal(feedback, ok)
	}
	if _, ok := ledger.TakeConsumed(); ok {
		t.Fatal("duplicate feedback")
	}
	requireUsage(t, ledger, Usage{RetainedSessions: 1})
}

func TestOnlyCompleteEnvelopeSizesAreAccepted(t *testing.T) {
	ledger := testLedger(t, testConfig())
	session := openSession(t, ledger, 1)
	for _, size := range []uint64{0, v2.OpaqueRouteHeaderBytes, maximumEnvelopeBytes + 1, ^uint64(0)} {
		_, err := ledger.Receive(session, size)
		requireError(t, err, ErrFrameSize)
		requireUsage(t, ledger, Usage{RetainedSessions: 1})
	}
	receipt := receiveFrame(t, ledger, session, maximumEnvelopeBytes)
	requireError(t, ledger.Consume(receipt), nil)
	feedback, ok := ledger.TakeConsumed()
	if !ok || feedback.TotalEncodedBytes != maximumEnvelopeBytes {
		t.Fatal("maximum envelope lost its header", feedback, ok)
	}
}

func TestConfigurationBoundsMetadataWithoutPreallocatingQueues(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.ConnectionID = 0 },
		func(c *Config) { c.MaxSessions = 0 },
		func(c *Config) { c.MaxSessions = -1 },
		func(c *Config) { c.Capacity.Frames = 0 },
		func(c *Config) { c.Capacity.EncodedBytes = 0 },
	} {
		config := testConfig()
		mutate(&config)
		_, err := New(config)
		requireError(t, err, ErrConfig)
	}
	config := testConfig()
	config.MaxSessions = 1
	// Handshake capacities need not themselves be possible envelope totals.
	config.Capacity = v2.ReceiveCapacity{Frames: ^uint64(0), EncodedBytes: 1}
	ledger := testLedger(t, config)
	_, err := ledger.Open(v2.RelaySessionID{})
	requireError(t, err, ErrSession)
	session := openSession(t, ledger, 1)
	_, err = ledger.Open(testID(1))
	requireError(t, err, ErrSession)
	requireError(t, ledger.Retire(session), nil)
	_, err = ledger.Open(testID(2))
	requireError(t, err, ErrSessionLimit)
	requireError(t, ledger.Forget(session), ErrSettlementPending)
	requireError(t, ledger.Notice(session, v2.SettlementNotice{RelaySessionID: testID(1)}), nil)
	if _, ok := ledger.TakeSettlement(session); !ok {
		t.Fatal("empty retired session could not settle")
	}
	requireError(t, ledger.Forget(session), nil)
	openSession(t, ledger, 2)
	requireUsage(t, ledger, Usage{RetainedSessions: 1})
}
