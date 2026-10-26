package sendersession

import (
	"context"
	"errors"
	sessioncontract "github.com/windshare/windshare/core/session"

	"testing"
)

func TestSenderLifecycleRejectsMissingReceiverAndContextAuthority(t *testing.T) {
	var nilRuntime *Runtime
	if err := nilRuntime.Stop(context.Background(), "stop"); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
		t.Fatalf("nil runtime stop error=%v", err)
	}
	if err := nilRuntime.BeginStop(context.Background(), "stop"); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
		t.Fatalf("nil runtime begin-stop error=%v", err)
	}
	if err := nilRuntime.WaitStopped(context.Background()); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
		t.Fatalf("nil runtime wait-stop error=%v", err)
	}
	nilRuntime.BeginClose()
	nilRuntime.WaitClosed()

	runtime := &Runtime{}
	// A named nil context is deliberate invalid-input evidence; production
	// lifecycle calls must always carry caller-owned cancellation authority.
	var missingContext context.Context
	if err := runtime.Stop(missingContext, "stop"); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil stop context error=%v", err)
	}
	if err := runtime.BeginStop(missingContext, "stop"); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil begin-stop context error=%v", err)
	}
	if err := runtime.WaitStopped(missingContext); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil wait-stop context error=%v", err)
	}

	var nilFactory *Factory
	if err := nilFactory.Stop(context.Background(), "stop"); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
		t.Fatalf("nil factory stop error=%v", err)
	}
	if err := nilFactory.BeginStop("stop"); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
		t.Fatalf("nil factory begin-stop error=%v", err)
	}
	if err := (&Factory{}).Stop(missingContext, "stop"); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil factory stop context error=%v", err)
	}
	if normalized, err := normalizeTerminalMessage(""); err != nil || normalized != "Sender stopped" {
		t.Fatalf("default terminal message=%q error=%v", normalized, err)
	}
}

func TestSenderWaitStoppedHonorsLifecycleAndJoinBoundaries(t *testing.T) {
	t.Run("before stop", func(t *testing.T) {
		runtime := &Runtime{lifecycle: testLifecycle{done: make(chan struct{})}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := runtime.WaitStopped(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-stop canceled wait error=%v", err)
		}
	})

	t.Run("started stop", func(t *testing.T) {
		stopFailure := errors.New("terminal fanout failed")
		runtime := &Runtime{
			lifecycle:   testLifecycle{done: make(chan struct{})},
			stopStarted: true,
			stopDone:    make(chan struct{}),
			stopErr:     stopFailure,
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := runtime.WaitStopped(ctx); !errors.Is(err, context.Canceled) || !errors.Is(err, stopFailure) {
			t.Fatalf("started-stop canceled wait error=%v", err)
		}
	})

	t.Run("composite join", func(t *testing.T) {
		if err := (&Runtime{}).waitComposite(context.Background()); err != nil {
			t.Fatalf("absent composite join error=%v", err)
		}
		done := make(chan struct{})
		runtime := &Runtime{compositeDone: done}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := runtime.waitComposite(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled composite join error=%v", err)
		}
		close(done)
		if err := runtime.waitComposite(context.Background()); err != nil {
			t.Fatalf("completed composite join error=%v", err)
		}
	})
}

func TestStopFactorySessionPreservesRuntimeAfterMessageValidationFailure(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	if err := stopFactorySession(context.Background(), sender, "e\u0301"); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("invalid factory terminal message error=%v", err)
	}
	select {
	case <-sender.Done():
		t.Fatal("message validation failure closed a still-usable session")
	default:
	}
}
