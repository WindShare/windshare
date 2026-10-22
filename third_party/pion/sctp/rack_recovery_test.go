package sctp

import (
	"testing"
	"time"

	"github.com/pion/logging"
)

func TestRackLostRetransmission(t *testing.T) {
	for _, trigger := range []string{"sack", "timer"} {
		for _, test := range []struct {
			name                string
			sends               uint32
			age                 time.Duration
			queued, acked, want bool
		}{
			{"original", 1, 100 * time.Millisecond, false, false, true},
			{"retry", 2, 100 * time.Millisecond, false, false, true},
			{"repeated-retry", 3, 100 * time.Millisecond, false, false, true},
			{"recent-retry", 2, 5 * time.Millisecond, false, false, false},
			{"already-queued", 2, 100 * time.Millisecond, true, false, true},
			{"already-acked", 2, 100 * time.Millisecond, false, true, false},
		} {
			t.Run(trigger+"/"+test.name, func(t *testing.T) {
				a := newRecoveryTestAssociation(t)
				a.lock.Lock()
				defer a.lock.Unlock()
				now := time.Now()
				a.rackReorderingSeen = true
				a.rackReoWnd = 10 * time.Millisecond
				a.rackMinRTT = 40 * time.Millisecond
				c := recoveryTestChunk(100, now.Add(-test.age))
				c.nSent = test.sends
				c.retransmit = test.queued
				c.acked = test.acked
				a.inflightQueue.pushNoCheck(c)
				a.rackInsert(c)
				delivered := now.Add(-50 * time.Millisecond)
				if trigger == "sack" {
					a.onRackAfterSACK(true, delivered, 101, &chunkSelectiveAck{})
				} else {
					a.rackDeliveredTime = delivered
					a.onRackTimeoutLocked()
				}
				if c.retransmit != test.want {
					t.Fatalf("retransmit=%v, want %v for latest transmission age %s", c.retransmit, test.want, test.age)
				}
			})
		}
	}
}

func TestRackDeliveryAmbiguity(t *testing.T) {
	for _, selective := range []bool{false, true} {
		for _, test := range []struct {
			name  string
			age   time.Duration
			valid bool
		}{
			{"old-ack-arrives-before-minimum-rtt", time.Millisecond, false},
			{"credible-retry-ack", 2 * time.Second, true},
		} {
			name := test.name + "/cumulative"
			if selective {
				name = test.name + "/selective"
			}
			t.Run(name, func(t *testing.T) {
				a := newRecoveryTestAssociation(t)
				a.lock.Lock()
				defer a.lock.Unlock()
				a.rackMinRTT = 40 * time.Millisecond
				a.rackReorderingSeen = true
				a.rackReoWnd = 10 * time.Millisecond
				now := time.Now()
				retryTSN, otherTSN := uint32(100), uint32(101)
				if selective {
					retryTSN, otherTSN = 101, 100
				}
				retry := recoveryTestChunk(retryTSN, now.Add(-test.age))
				retry.nSent = 2
				other := recoveryTestChunk(otherTSN, now.Add(-3*time.Second))
				if selective {
					a.inflightQueue.pushNoCheck(other)
					a.inflightQueue.pushNoCheck(retry)
				} else {
					a.inflightQueue.pushNoCheck(retry)
					a.inflightQueue.pushNoCheck(other)
				}
				a.rackInsert(other)
				a.rackInsert(retry)
				sack := &chunkSelectiveAck{cumulativeTSNAck: 100, advertisedReceiverWindowCredit: 1024 * 1024}
				if selective {
					sack.cumulativeTSNAck = 99
					sack.gapAckBlocks = []gapAckBlock{{start: 2, end: 2}}
				}
				if err := a.handleSack(sack); err != nil {
					t.Fatal(err)
				}
				if found := !a.rackDeliveredTime.IsZero(); found != test.valid {
					t.Fatalf("retry used as delivery evidence=%v, want %v", found, test.valid)
				}
				if other.retransmit != test.valid {
					t.Fatalf("older chunk marked lost=%v, want %v", other.retransmit, test.valid)
				}
			})
		}
	}
}

func TestRackAcknowledgedTransmissionValidity(t *testing.T) {
	now := time.Unix(100, 0)
	for _, test := range []struct {
		name        string
		sends       uint32
		minRTT, age time.Duration
		want        bool
	}{
		{"original-without-rtt", 1, 0, 0, true},
		{"retry-without-rtt", 2, 0, time.Second, false},
		{"retry-before-minimum-rtt", 2, time.Second, time.Second - time.Nanosecond, false},
		{"retry-at-minimum-rtt", 2, time.Second, time.Second, true},
		{"third-send-before-minimum-rtt", 3, time.Second, time.Millisecond, false},
		{"third-send-after-minimum-rtt", 3, time.Second, 2 * time.Second, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := &Association{rackMinRTT: test.minRTT}
			chunk := &chunkPayloadData{nSent: test.sends, since: now.Add(-test.age)}
			if got := a.rackAcknowledgedTransmissionValid(chunk, now); got != test.want {
				t.Fatalf("valid delivery evidence = %v, want %v", got, test.want)
			}
		})
	}
}

func newRecoveryTestAssociation(t *testing.T) *Association {
	t.Helper()
	// No socket or read/write loops are needed to exercise the loss state machine.
	a := createAssociationFromConfigWithTsn(&Config{LoggerFactory: logging.NewDefaultLoggerFactory()}, 100)
	a.setState(established)
	a.myNextTSN = 102
	a.srtt.Store(float64(100))
	t.Cleanup(func() {
		a.lock.Lock()
		defer a.lock.Unlock()
		a.setState(closed)
		close(a.closeWriteLoopCh)
		a.closeAllTimers()
	})
	return a
}

func recoveryTestChunk(tsn uint32, sent time.Time) *chunkPayloadData {
	return &chunkPayloadData{
		streamIdentifier:     1,
		streamSequenceNumber: 1,
		beginningFragment:    true,
		endingFragment:       true,
		userData:             []byte("x"),
		tsn:                  tsn,
		since:                sent,
		nSent:                1,
	}
}
