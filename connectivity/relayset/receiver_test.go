package relayset

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type receiverFakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (c *receiverFakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *receiverFakeClock) Wait(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	return nil
}
func TestReceiverInitialStartingUsesBoundedClockAndAllFailuresSettle(t *testing.T) {
	raw := make([]byte, v2.ShareIDBytes)
	raw[0] = 1
	capability := link.Link{ShareID: base64.RawURLEncoding.EncodeToString(raw), Relays: []string{"one", "two"}}
	clock := &receiverFakeClock{now: time.Unix(1, 0)}
	var calls atomic.Int32
	rejection := errors.New("endpoint offline")
	recovery, err := NewReceiverRecovery(ReceiverRecoveryOptions{
		Clock: clock, InitialWait: time.Second, Jitter: func(delay time.Duration) time.Duration { return delay },
	})
	if err != nil {
		t.Fatal(err)
	}
	set, err := recovery.Join(context.Background(), ReceiverConfig{Receiver: liveshare.ReceiverConfig{Capability: capability}, Dial: func(context.Context, relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error) {
		if calls.Add(1) <= 2 {
			return nil, &relayv2.RelayError{Code: v2.ErrorStarting}
		}
		return nil, rejection
	}})
	if set != nil || !errors.Is(err, rejection) || !errors.Is(err, ErrReceiverUnavailable) {
		t.Fatal(set, err)
	}
	if len(clock.waits) != 3 || clock.waits[0] != receiverRetryDelay || clock.Now() != time.Unix(2, 0) {
		t.Fatal(clock.waits)
	}
	var missingContext context.Context // Constructors reject missing lifetime ownership.
	if _, err = NewReceiver(missingContext, ReceiverConfig{Receiver: liveshare.ReceiverConfig{Capability: capability}}); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err = NewReceiver(context.Background(), ReceiverConfig{Receiver: liveshare.ReceiverConfig{Capability: link.Link{Relays: []string{"one"}, ShareID: "invalid!"}}}); err == nil {
		t.Fatal("bad share accepted")
	}
}

func TestReceiverCancellationJoinsPendingDial(t *testing.T) {
	raw := make([]byte, v2.ShareIDBytes)
	raw[0] = 1
	entered := make(chan struct{})
	set, err := NewReceiver(context.Background(), ReceiverConfig{Receiver: liveshare.ReceiverConfig{Capability: link.Link{ShareID: base64.RawURLEncoding.EncodeToString(raw), Relays: []string{"one"}}}, Dial: func(ctx context.Context, _ relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err = set.WaitReady(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	set.Close()
	set.Close()
	if _, _, err = set.WaitReady(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
