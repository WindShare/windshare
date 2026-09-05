package relayset

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/transport/relayv2"
)

type endpoint struct {
	inbound          chan *relayv2.Channel
	failed           chan struct{}
	mu               sync.Mutex
	stopped, cleaned bool
}

func newEndpoint() *endpoint {
	return &endpoint{inbound: make(chan *relayv2.Channel, 1), failed: make(chan struct{})}
}
func (e *endpoint) Accept(ctx context.Context) (*relayv2.Channel, error) {
	select {
	case channel := <-e.inbound:
		return channel, nil
	case <-e.failed:
		return nil, errors.New("endpoint lost")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (e *endpoint) StopRecovery() { e.mu.Lock(); e.stopped = true; e.mu.Unlock() }
func (e *endpoint) Cleanup(context.Context) error {
	e.mu.Lock()
	e.cleaned = true
	e.mu.Unlock()
	return nil
}
func waitValue[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal("event missing")
		var zero T
		return zero
	}
}
func cleanupSet(t *testing.T, set *Sender) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := set.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestSenderFirstRegistrationDoesNotWaitForSlowEndpoint(t *testing.T) {
	started := make(chan string, 2)
	fast := newEndpoint()
	slow := newEndpoint()
	release := make(chan struct{})
	set, err := NewSender(context.Background(), []string{"fast", "slow"}, func(ctx context.Context, url string) (SenderEndpoint, error) {
		started <- url
		if url == "fast" {
			return fast, nil
		}
		select {
		case <-release:
			return slow, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	waitValue(t, started)
	waitValue(t, started)
	ready := make(chan error, 1)
	go func() { ready <- set.WaitReady(context.Background()) }()
	if err := waitValue(t, ready); err != nil {
		t.Fatal(err)
	}
	close(release)
	// The first endpoint can disappear without revoking the shared ingress or
	// sessions already served through another relay/direct lane.
	close(fast.failed)
	channel := &relayv2.Channel{}
	slow.inbound <- channel
	got := make(chan *relayv2.Channel, 1)
	go func() { value, _ := set.Accept(context.Background()); got <- value }()
	if waitValue(t, got) != channel {
		t.Fatal("independent endpoint was not served")
	}
}

func TestSenderAllInitialFailuresAreReportedTogether(t *testing.T) {
	unavailable := errors.New("relay unavailable")
	set, err := NewSender(context.Background(), []string{"one", "two"}, func(context.Context, string) (SenderEndpoint, error) { return nil, unavailable })
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := set.WaitReady(ctx); !errors.Is(err, unavailable) {
		t.Fatalf("failure=%v", err)
	}
}

func TestSenderCleanupJoinsLateDialAndCleansEachEndpoint(t *testing.T) {
	slowStarted := make(chan struct{})
	slow := newEndpoint()
	fast := newEndpoint()
	set, err := NewSender(context.Background(), []string{"fast", "late"}, func(ctx context.Context, url string) (SenderEndpoint, error) {
		if url == "fast" {
			return fast, nil
		}
		close(slowStarted)
		<-ctx.Done()
		// A dial may complete successfully concurrently with cancellation.
		return slow, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitValue(t, slowStarted)
	if err := set.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	cleanupSet(t, set)
	cleanupSet(t, set)
	for _, e := range []*endpoint{fast, slow} {
		e.mu.Lock()
		stopped, cleaned := e.stopped, e.cleaned
		e.mu.Unlock()
		if !stopped || !cleaned {
			t.Fatal("late endpoint escaped final ownership snapshot")
		}
	}
	if _, err := set.Accept(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal("stopped set accepted ingress")
	}
}

func TestSenderIngressRemainsOwnedAfterEveryEndpointFails(t *testing.T) {
	e := newEndpoint()
	set, _ := NewSender(context.Background(), []string{"relay"}, func(context.Context, string) (SenderEndpoint, error) { return e, nil })
	defer cleanupSet(t, set)
	if err := set.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(e.failed)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := set.Accept(ctx); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("endpoint failure revoked ingress: %v", err)
	default:
	}
	cancel()
	if !errors.Is(waitValue(t, done), context.Canceled) {
		t.Fatal("caller cancellation lost")
	}
}

func TestSenderSetRejectsUnboundedOrAmbiguousConfiguration(t *testing.T) {
	dial := func(context.Context, string) (SenderEndpoint, error) {
		t.Fatal("invalid configuration dialed")
		return nil, nil
	}
	for _, urls := range [][]string{nil, {""}, {"same", "same"}, make([]string, MaximumEndpoints+1)} {
		if _, err := NewSender(context.Background(), urls, dial); !errors.Is(err, ErrConfig) {
			t.Fatal("invalid set accepted")
		}
	}
	if _, err := NewSender(nil, []string{"valid"}, dial); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
	if _, err := NewSender(context.Background(), []string{"valid"}, nil); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Sender{ready: make(chan struct{})}
	if !errors.Is(s.WaitReady(ctx), context.Canceled) {
		t.Fatal("ready ignored caller cancellation")
	}
}
