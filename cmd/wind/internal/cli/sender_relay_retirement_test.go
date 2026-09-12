package cli

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"weak"

	"github.com/windshare/windshare/transport/relayv2"
)

type retiringSenderRelayEndpoint struct {
	stream          chan relayv2.LifecycleTrace
	done            chan struct{}
	closeStarted    chan struct{}
	deferTerminal   bool
	closeOnce       sync.Once
	terminalOnce    sync.Once
	completionCalls *atomic.Int32
}

func newRetiringSenderRelayEndpoint(calls *atomic.Int32) *retiringSenderRelayEndpoint {
	return &retiringSenderRelayEndpoint{
		stream: make(chan relayv2.LifecycleTrace, 1), done: make(chan struct{}),
		closeStarted: make(chan struct{}), completionCalls: calls,
	}
}

func (*retiringSenderRelayEndpoint) Accept(context.Context) (*relayv2.Channel, error) {
	return nil, relayv2.ErrClosed
}

func (endpoint *retiringSenderRelayEndpoint) Close() error {
	endpoint.closeOnce.Do(func() {
		close(endpoint.closeStarted)
		if !endpoint.deferTerminal {
			endpoint.finish()
		}
	})
	return nil
}

func (endpoint *retiringSenderRelayEndpoint) finish() {
	endpoint.terminalOnce.Do(func() {
		endpoint.stream <- relayv2.LifecycleTrace{
			LinkID: 1, OperationID: 1, Stage: relayv2.LifecycleLinkClosed,
			RetirementSource: relayv2.LifecycleRetirementLocalClose,
			Cause:            relayv2.LifecycleCauseNone, DrainCause: relayv2.LifecycleCauseNone,
		}
		close(endpoint.stream)
		close(endpoint.done)
	})
}

func (endpoint *retiringSenderRelayEndpoint) Done() <-chan struct{} { return endpoint.done }

func (endpoint *retiringSenderRelayEndpoint) LifecycleTrace() <-chan relayv2.LifecycleTrace {
	return endpoint.stream
}

func (endpoint *retiringSenderRelayEndpoint) CompleteObservations() relayv2.LifecycleObservationCompletion {
	endpoint.completionCalls.Add(1)
	return relayv2.LifecycleObservationCompletion{
		Enqueued: 1, Loss: relayv2.LifecycleObservationLoss{CapacityDropped: 2},
	}
}

func TestSenderRelayRetirementBoundsConnectionsAndReadersAcrossRecovery(t *testing.T) {
	const recoveryCount = 64
	var completionCalls atomic.Int32
	var activeReaders atomic.Int32
	initial := newRetiringSenderRelayEndpoint(&completionCalls)
	retired := []weak.Pointer[retiringSenderRelayEndpoint]{weak.Make(initial)}
	dialer := &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (senderRelayConnection, error) {
		if got := activeReaders.Load(); got != 0 {
			t.Fatalf("dial retained %d readers from previous connections", got)
		}
		next := newRetiringSenderRelayEndpoint(&completionCalls)
		retired = append(retired, weak.Make(next))
		return newSenderRelayConnection(next), nil
	}}
	config := newSenderRelayTestConfig(t, initial, dialer, newSenderRelayTestClock())
	observations := newShareObservations(&shareRecordingEmitter{detailed: true})
	cleanupProtocolObservations(t, observations.protocol)
	config.observeConnection = func(connection senderRelayConnection) func() {
		finish := observations.attachRelayStream(connection.LifecycleTrace())
		activeReaders.Add(1)
		return func() {
			finish()
			activeReaders.Add(-1)
		}
	}
	lifecycle, err := newSenderRelayLifecycle(config)
	if err != nil {
		t.Fatal(err)
	}
	for index := range recoveryCount {
		if err := lifecycle.recover(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := activeReaders.Load(); got != 1 {
			t.Fatalf("recovery %d retained %d readers, want only current", index, got)
		}
		if got := completionCalls.Load(); got != int32(index+1) {
			t.Fatalf("recovery %d completed %d sources", index, got)
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := lifecycle.Cleanup(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup = %v", err)
	}
	want := relayv2.LifecycleObservationCompletion{
		Enqueued: recoveryCount + 1,
		Loss:     relayv2.LifecycleObservationLoss{CapacityDropped: 2 * (recoveryCount + 1)},
	}
	for range 2 {
		if got := lifecycle.CompleteObservations(); got != want {
			t.Fatalf("completion = %+v, want %+v", got, want)
		}
	}
	if got := completionCalls.Load(); got != recoveryCount+1 || activeReaders.Load() != 0 {
		t.Fatalf("completed sources=%d active readers=%d", got, activeReaders.Load())
	}
	// The lifecycle and diagnostics owner stay live while all retired transport
	// objects become collectible; checking only Close would miss the old leak.
	runtime.GC()
	for index, reference := range retired {
		if reference.Value() != nil {
			t.Fatalf("retired connection %d is still reachable", index)
		}
	}
	runtime.KeepAlive(lifecycle)
	runtime.KeepAlive(observations)
}

func TestSenderRelayRetirementWaitsForTerminalObservation(t *testing.T) {
	var completionCalls atomic.Int32
	initial := newRetiringSenderRelayEndpoint(&completionCalls)
	initial.deferTerminal = true
	t.Cleanup(initial.finish)
	next := newRetiringSenderRelayEndpoint(&completionCalls)
	t.Cleanup(func() { _ = next.Close() })
	lifecycle := newSenderRelayTestLifecycle(t, initial, &senderRelayTestDialer{
		dial: func(context.Context, relayv2.SenderConfig) (senderRelayConnection, error) {
			return newSenderRelayConnection(next), nil
		},
	}, newSenderRelayTestClock())
	result := make(chan error, 1)
	go func() { result <- lifecycle.recover(t.Context()) }()
	senderRelayAwaitSignal(t, initial.closeStarted, "old transport close")
	if completionCalls.Load() != 0 {
		t.Fatal("producer diagnostics were cut before terminal retirement")
	}
	initial.finish()
	if err := senderRelayAwaitError(t, result); err != nil {
		t.Fatal(err)
	}
	if completionCalls.Load() != 1 {
		t.Fatal("terminal producer counters were not folded into history")
	}
}

func TestSenderRelayCanceledLateDialRetiresDiagnostics(t *testing.T) {
	var completionCalls atomic.Int32
	initial := newRetiringSenderRelayEndpoint(&completionCalls)
	late := newRetiringSenderRelayEndpoint(&completionCalls)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lifecycle := newSenderRelayTestLifecycle(t, initial, &senderRelayTestDialer{
		dial: func(context.Context, relayv2.SenderConfig) (senderRelayConnection, error) {
			cancel()
			return newSenderRelayConnection(late), nil
		},
	}, newSenderRelayTestClock())
	var retiredReaders atomic.Int32
	lifecycle.config.observeConnection = func(senderRelayConnection) func() {
		return func() { retiredReaders.Add(1) }
	}
	if err := lifecycle.recover(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("late dial recovery = %v", err)
	}
	got := lifecycle.CompleteObservations()
	if got.Enqueued != 2 || got.Loss.CapacityDropped != 4 || completionCalls.Load() != 2 || retiredReaders.Load() != 1 {
		t.Fatalf("lost late-dial diagnostics: %+v calls=%d readers=%d", got, completionCalls.Load(), retiredReaders.Load())
	}
	if lifecycle.connection.valid() {
		t.Fatal("canceled dial left installed authority")
	}
}
