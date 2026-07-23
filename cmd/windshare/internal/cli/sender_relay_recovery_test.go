package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestSenderRelayRecoveryRetriesUnexpectedDisconnectWithBackoff(t *testing.T) {
	disconnect := errors.New("unexpected relay disconnect")
	transient := errors.New("relay temporarily unavailable")
	accepted := new(relayv2.Channel)
	initial := &senderRelayTestEndpoint{
		accept: func(context.Context) (*relayv2.Channel, error) {
			return nil, disconnect
		},
	}
	recovered := &senderRelayTestEndpoint{
		accept: func(context.Context) (*relayv2.Channel, error) {
			return accepted, nil
		},
	}
	clock := newSenderRelayTestClock()
	var attempts atomic.Int32
	dialer := &senderRelayTestDialer{
		dial: func(context.Context, relayv2.SenderConfig) (senderRelayConnection, error) {
			if attempts.Add(1) == 1 {
				return senderRelayConnection{}, transient
			}
			return newSenderRelayConnection(recovered), nil
		},
	}
	lifecycle := newSenderRelayTestLifecycle(t, initial, dialer, clock)

	channel, err := lifecycle.Accept(context.Background())
	if err != nil {
		t.Fatalf("Accept after transient disconnect: %v", err)
	}
	if channel != accepted {
		t.Fatal("Accept did not use the recovered relay connection")
	}
	if got := initial.closeCalls.Load(); got != 1 {
		t.Fatalf("failed connection close calls = %d, want 1", got)
	}
	if got := recovered.closeCalls.Load(); got != 0 {
		t.Fatalf("recovered connection close calls = %d, want 0", got)
	}
	if got := clock.Waits(); len(got) != 1 || got[0] != senderRelayRetryInitial {
		t.Fatalf("recovery waits = %v, want [%v]", got, senderRelayRetryInitial)
	}

	configs, deadlines := dialer.Snapshot()
	if len(configs) != 2 || attempts.Load() != 2 {
		t.Fatalf("resume dial attempts = %d, want 2", len(configs))
	}
	for index, config := range configs {
		if config.Init != lifecycle.resume {
			t.Fatalf("dial %d did not use the authenticated resume init", index)
		}
		if config.ResumeToken != lifecycle.config.resumeToken {
			t.Fatalf("dial %d did not use the resume token", index)
		}
		if len(config.Descriptor) != 0 {
			t.Fatalf("dial %d sent a descriptor during resume", index)
		}
		if deadlines[index].IsZero() {
			t.Fatalf("dial %d had no absolute recovery deadline", index)
		}
	}
	if !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("dial attempts used different recovery deadlines: %v", deadlines)
	}
}

func TestSenderRelayRecoveryExpiresFixedBudget(t *testing.T) {
	disconnect := errors.New("unexpected relay disconnect")
	unavailable := errors.New("relay remains unavailable")
	initial := &senderRelayTestEndpoint{
		accept: func(context.Context) (*relayv2.Channel, error) {
			return nil, disconnect
		},
	}
	clock := newSenderRelayTestClock()
	dialer := &senderRelayTestDialer{
		dial: func(context.Context, relayv2.SenderConfig) (senderRelayConnection, error) {
			return senderRelayConnection{}, unavailable
		},
	}
	lifecycle := newSenderRelayTestLifecycle(t, initial, dialer, clock)

	_, err := lifecycle.Accept(context.Background())
	if !errors.Is(err, unavailable) {
		t.Fatalf("Accept error = %v, want final dial error", err)
	}
	waits := clock.Waits()
	if len(waits) != 57 {
		t.Fatalf("recovery wait count = %d, want 57", len(waits))
	}
	wantRamp := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	}
	for index, want := range wantRamp {
		if waits[index] != want {
			t.Fatalf("recovery wait %d = %v, want %v", index, waits[index], want)
		}
	}
	for index, wait := range waits[len(wantRamp):] {
		if wait != senderRelayRetryMaximum {
			t.Fatalf("capped recovery wait %d = %v, want %v", index, wait, senderRelayRetryMaximum)
		}
	}
	var elapsed time.Duration
	for _, wait := range waits {
		elapsed += wait
	}
	if elapsed != 54*time.Second+500*time.Millisecond {
		t.Fatalf("recovery elapsed time = %v, want 54.5s", elapsed)
	}
	configs, _ := dialer.Snapshot()
	if len(configs) != len(waits)+1 {
		t.Fatalf("resume dial attempts = %d, want %d", len(configs), len(waits)+1)
	}
	if got := initial.closeCalls.Load(); got != 1 {
		t.Fatalf("failed connection close calls = %d, want 1", got)
	}
	lifecycle.mu.Lock()
	installed := lifecycle.connection.valid()
	lifecycle.mu.Unlock()
	if installed {
		t.Fatal("failed recovery left a relay connection installed")
	}
}

func TestSenderRelayAcceptCancellationClosesLateDialSuccess(t *testing.T) {
	disconnect := errors.New("unexpected relay disconnect")
	initial := &senderRelayTestEndpoint{
		accept: func(context.Context) (*relayv2.Channel, error) {
			return nil, disconnect
		},
	}
	late := &senderRelayTestEndpoint{
		accept: func(context.Context) (*relayv2.Channel, error) {
			return nil, errors.New("caller-canceled relay connection was installed")
		},
	}
	dialStarted := make(chan struct{})
	dialer := &senderRelayTestDialer{
		dial: func(ctx context.Context, _ relayv2.SenderConfig) (senderRelayConnection, error) {
			close(dialStarted)
			// A completed handshake may surface at the same instant its context
			// is canceled, so recovery must close the result before installation.
			<-ctx.Done()
			return newSenderRelayConnection(late), nil
		},
	}
	lifecycle := newSenderRelayTestLifecycle(t, initial, dialer, newSenderRelayTestClock())
	acceptContext, cancelAccept := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := lifecycle.Accept(acceptContext)
		result <- err
	}()

	senderRelayAwaitSignal(t, dialStarted, "caller-bound resume dial")
	cancelAccept()
	err := senderRelayAwaitError(t, result)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Accept error = %v, want caller cancellation", err)
	}
	if got := initial.closeCalls.Load(); got != 1 {
		t.Fatalf("failed connection close calls = %d, want 1", got)
	}
	if got := late.closeCalls.Load(); got != 1 {
		t.Fatalf("caller-canceled late connection close calls = %d, want 1", got)
	}
	if got := late.acceptCalls.Load(); got != 0 {
		t.Fatalf("caller-canceled late connection Accept calls = %d, want 0", got)
	}
	lifecycle.mu.Lock()
	installed := lifecycle.connection.valid()
	stopping := lifecycle.stopping
	lifecycle.mu.Unlock()
	if installed || stopping {
		t.Fatalf("caller cancellation state: installed=%v stopping=%v", installed, stopping)
	}
}

func TestSenderRelayStopRecoveryInterruptsBackoffWithoutResume(t *testing.T) {
	disconnect := errors.New("unexpected relay disconnect")
	transient := errors.New("relay temporarily unavailable")
	initial := &senderRelayTestEndpoint{
		accept: func(context.Context) (*relayv2.Channel, error) {
			return nil, disconnect
		},
	}
	waiting := make(chan struct{})
	clock := newSenderRelayTestClock()
	clock.wait = func(ctx context.Context, _ time.Duration) error {
		close(waiting)
		<-ctx.Done()
		return ctx.Err()
	}
	dialer := &senderRelayTestDialer{
		dial: func(context.Context, relayv2.SenderConfig) (senderRelayConnection, error) {
			return senderRelayConnection{}, transient
		},
	}
	lifecycle := newSenderRelayTestLifecycle(t, initial, dialer, clock)
	result := make(chan error, 1)
	go func() {
		_, err := lifecycle.Accept(context.Background())
		result <- err
	}()

	senderRelayAwaitSignal(t, waiting, "recovery backoff")
	lifecycle.StopRecovery()
	err := senderRelayAwaitError(t, result)
	if !errors.Is(err, errSenderRelayRecoveryStopped) {
		t.Fatalf("Accept error = %v, want explicit recovery stop", err)
	}
	configs, _ := dialer.Snapshot()
	if len(configs) != 1 {
		t.Fatalf("resume dial attempts after stop = %d, want 1", len(configs))
	}
	if got := initial.closeCalls.Load(); got != 1 {
		t.Fatalf("failed connection close calls = %d, want 1", got)
	}
}

func TestSenderRelayStopWinsDialSuccessAndClosesLateConnection(t *testing.T) {
	disconnect := errors.New("unexpected relay disconnect")
	initial := &senderRelayTestEndpoint{
		accept: func(context.Context) (*relayv2.Channel, error) {
			return nil, disconnect
		},
	}
	late := &senderRelayTestEndpoint{
		accept: func(context.Context) (*relayv2.Channel, error) {
			return nil, errors.New("late relay connection was installed")
		},
	}
	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})
	dialer := &senderRelayTestDialer{
		dial: func(context.Context, relayv2.SenderConfig) (senderRelayConnection, error) {
			close(dialStarted)
			// A transport can finish its handshake just after cancellation; the
			// lifecycle must arbitrate installation rather than trust the dial.
			<-releaseDial
			return newSenderRelayConnection(late), nil
		},
	}
	lifecycle := newSenderRelayTestLifecycle(t, initial, dialer, newSenderRelayTestClock())
	result := make(chan error, 1)
	go func() {
		_, err := lifecycle.Accept(context.Background())
		result <- err
	}()

	senderRelayAwaitSignal(t, dialStarted, "resume dial")
	lifecycle.StopRecovery()
	close(releaseDial)
	err := senderRelayAwaitError(t, result)
	if !errors.Is(err, errSenderRelayRecoveryStopped) {
		t.Fatalf("Accept error = %v, want explicit recovery stop", err)
	}
	if got := late.closeCalls.Load(); got != 1 {
		t.Fatalf("late connection close calls = %d, want 1", got)
	}
	if got := late.acceptCalls.Load(); got != 0 {
		t.Fatalf("late connection Accept calls = %d, want 0", got)
	}
	if got := initial.closeCalls.Load(); got != 1 {
		t.Fatalf("failed connection close calls = %d, want 1", got)
	}
	lifecycle.mu.Lock()
	installed := lifecycle.connection.valid()
	lifecycle.mu.Unlock()
	if installed {
		t.Fatal("late relay connection was installed after explicit stop")
	}
}

func TestSenderRelayDefaultRecoveryDependenciesHonorContext(t *testing.T) {
	clock := wallSenderRelayRecoveryClock{}
	before := time.Now()
	now := clock.Now()
	after := time.Now()
	if now.Before(before) || now.After(after) {
		t.Fatalf("wall recovery time = %v, outside [%v, %v]", now, before, after)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := clock.Wait(canceled, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wall recovery wait = %v", err)
	}
	connection, err := (relayV2SenderDialer{}).Dial(context.Background(), relayv2.SenderConfig{})
	if err == nil || connection.valid() {
		t.Fatalf("invalid default dial = (%v, %v)", connection.valid(), err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close empty connection: %v", err)
	}
}

func TestSenderRelayCleanupKeepsDefaultDependenciesAndClosesOnce(t *testing.T) {
	initial := &senderRelayTestEndpoint{
		accept: func(context.Context) (*relayv2.Channel, error) {
			return nil, errors.New("unused")
		},
	}
	lifecycle := newSenderRelayTestLifecycle(t, initial, nil, nil)
	if _, ok := lifecycle.config.dialer.(relayV2SenderDialer); !ok {
		t.Fatalf("default dialer type = %T", lifecycle.config.dialer)
	}
	if _, ok := lifecycle.config.clock.(wallSenderRelayRecoveryClock); !ok {
		t.Fatalf("default recovery clock type = %T", lifecycle.config.clock)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	firstErr := lifecycle.Cleanup(canceled)
	if !errors.Is(firstErr, context.Canceled) {
		t.Fatalf("Cleanup error = %v, want canceled STOP", firstErr)
	}
	secondErr := lifecycle.Cleanup(context.Background())
	if !errors.Is(secondErr, context.Canceled) {
		t.Fatalf("idempotent Cleanup error = %v, want original result", secondErr)
	}
	if got := initial.closeCalls.Load(); got != 1 {
		t.Fatalf("cleanup connection close calls = %d, want 1", got)
	}
	lifecycle.mu.Lock()
	installed := lifecycle.connection.valid()
	stopping := lifecycle.stopping
	lifecycle.mu.Unlock()
	if installed || !stopping {
		t.Fatalf("cleanup lifecycle state: installed=%v stopping=%v", installed, stopping)
	}
}

type senderRelayTestEndpoint struct {
	accept      func(context.Context) (*relayv2.Channel, error)
	acceptCalls atomic.Int32
	closeCalls  atomic.Int32
}

func (endpoint *senderRelayTestEndpoint) Accept(ctx context.Context) (*relayv2.Channel, error) {
	endpoint.acceptCalls.Add(1)
	return endpoint.accept(ctx)
}

func (endpoint *senderRelayTestEndpoint) Close() error {
	endpoint.closeCalls.Add(1)
	return nil
}

type senderRelayTestDialer struct {
	mu        sync.Mutex
	dial      func(context.Context, relayv2.SenderConfig) (senderRelayConnection, error)
	configs   []relayv2.SenderConfig
	deadlines []time.Time
}

func (dialer *senderRelayTestDialer) Dial(
	ctx context.Context,
	config relayv2.SenderConfig,
) (senderRelayConnection, error) {
	deadline, _ := ctx.Deadline()
	dialer.mu.Lock()
	dialer.configs = append(dialer.configs, config)
	dialer.deadlines = append(dialer.deadlines, deadline)
	dialer.mu.Unlock()
	return dialer.dial(ctx, config)
}

func (dialer *senderRelayTestDialer) Snapshot() ([]relayv2.SenderConfig, []time.Time) {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	return append([]relayv2.SenderConfig(nil), dialer.configs...),
		append([]time.Time(nil), dialer.deadlines...)
}

type senderRelayTestClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
	wait  func(context.Context, time.Duration) error
}

func newSenderRelayTestClock() *senderRelayTestClock {
	return &senderRelayTestClock{now: time.Unix(1_700_000_000, 0)}
}

func (clock *senderRelayTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *senderRelayTestClock) Wait(ctx context.Context, delay time.Duration) error {
	clock.mu.Lock()
	clock.waits = append(clock.waits, delay)
	wait := clock.wait
	if wait == nil {
		clock.now = clock.now.Add(delay)
	}
	clock.mu.Unlock()
	if wait != nil {
		return wait(ctx, delay)
	}
	return ctx.Err()
}

func (clock *senderRelayTestClock) Waits() []time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return append([]time.Duration(nil), clock.waits...)
}

func newSenderRelayTestLifecycle(
	t *testing.T,
	initial senderRelayEndpoint,
	dialer senderRelayDialer,
	clock senderRelayRecoveryClock,
) *senderRelayLifecycle {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed(senderRelayBytesFrom(0x20, ed25519.SeedSize))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	pkDigest := sha256.Sum256(append([]byte("windshare/v2 sender-key\x00"), publicKey...))
	var pkHash v2.PKHash
	copy(pkHash[:], pkDigest[:v2.PKHashBytes])
	shareDigest := sha256.Sum256(append([]byte("windshare/v2 share-id\x00"), pkHash[:]...))
	var shareID v2.ShareID
	copy(shareID[:], shareDigest[:v2.ShareIDBytes])
	var shareInstance v2.ShareInstance
	copy(shareInstance[:], senderRelayBytesFrom(1, v2.ShareInstanceBytes))
	var resumeToken v2.ResumeToken
	copy(resumeToken[:], senderRelayBytesFrom(0x40, v2.ResumeTokenBytes))
	descriptor := []byte("sender relay recovery test")
	fresh, err := relayv2.NewFreshRegisterInit(
		shareID,
		shareInstance,
		pkHash,
		descriptor,
		resumeToken,
	)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := newSenderRelayLifecycle(senderRelayLifecycleConfig{
		relayURL:    "https://relay.example",
		fresh:       fresh,
		resumeToken: resumeToken,
		privateKey:  privateKey,
		initial:     initial,
		dialer:      dialer,
		clock:       clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.config.initial != nil {
		t.Fatal("bootstrap relay connection remained reachable through recovery config")
	}
	return lifecycle
}

func senderRelayBytesFrom(first byte, length int) []byte {
	result := make([]byte, length)
	for index := range result {
		result[index] = first + byte(index)
	}
	return result
}

func senderRelayAwaitSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func senderRelayAwaitError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for relay recovery result")
		return nil
	}
}
