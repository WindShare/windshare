package sessionruntime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestSenderLifecycleRejectsMissingReceiverAndContextAuthority(t *testing.T) {
	var nilRuntime *SenderRuntime
	if err := nilRuntime.Stop(context.Background(), "stop"); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("nil runtime stop error=%v", err)
	}
	if err := nilRuntime.BeginStop(context.Background(), "stop"); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("nil runtime begin-stop error=%v", err)
	}
	if err := nilRuntime.WaitStopped(context.Background()); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("nil runtime wait-stop error=%v", err)
	}
	nilRuntime.BeginClose()
	nilRuntime.WaitClosed()

	runtime := &SenderRuntime{}
	if err := runtime.Stop(nil, "stop"); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil stop context error=%v", err)
	}
	if err := runtime.BeginStop(nil, "stop"); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil begin-stop context error=%v", err)
	}
	if err := runtime.WaitStopped(nil); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil wait-stop context error=%v", err)
	}

	var nilFactory *SenderFactory
	if err := nilFactory.Stop(context.Background(), "stop"); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("nil factory stop error=%v", err)
	}
	if err := nilFactory.BeginStop("stop"); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("nil factory begin-stop error=%v", err)
	}
	if err := (&SenderFactory{}).Stop(nil, "stop"); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil factory stop context error=%v", err)
	}
	if normalized, err := normalizeTerminalMessage(""); err != nil || normalized != "Sender stopped" {
		t.Fatalf("default terminal message=%q error=%v", normalized, err)
	}
}

func TestSenderWaitStoppedHonorsLifecycleAndJoinBoundaries(t *testing.T) {
	t.Run("before stop", func(t *testing.T) {
		runtime := &SenderRuntime{runtimeCore: &runtimeCore{done: make(chan struct{})}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := runtime.WaitStopped(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-stop canceled wait error=%v", err)
		}
	})

	t.Run("started stop", func(t *testing.T) {
		stopFailure := errors.New("terminal fanout failed")
		runtime := &SenderRuntime{
			runtimeCore: &runtimeCore{done: make(chan struct{})},
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
		if err := (&SenderRuntime{}).waitComposite(context.Background()); err != nil {
			t.Fatalf("absent composite join error=%v", err)
		}
		done := make(chan struct{})
		runtime := &SenderRuntime{compositeDone: done}
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

func TestWaitStoppedJoinsRuntimeThatClosedWithoutOrderedStop(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	sender.BeginClose()
	waitDone := make(chan error, 1)
	go func() { waitDone <- sender.WaitStopped(context.Background()) }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("naturally closed wait-stop error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait-stop did not join naturally closed composite ownership")
	}
}

func TestStopFactorySessionPreservesRuntimeAfterMessageValidationFailure(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	if err := stopFactorySession(context.Background(), sender, "e\u0301"); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("invalid factory terminal message error=%v", err)
	}
	select {
	case <-sender.Done():
		t.Fatal("message validation failure closed a still-usable session")
	default:
	}
}

func TestTerminalFanoutPreservesCallerCancellationBeforeAdmission(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	lane, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	stopWriterBeforeAdmission(t, lane.writer)
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: SessionStoppedCode, Message: "stop",
	})
	if err != nil {
		t.Fatal(err)
	}
	callerContext, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	outbound := senderOutbound{
		runtime:    runtime,
		privateKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
	}
	err = outbound.sendTerminalRecipients(
		context.Background(),
		callerContext,
		body,
		[]selectedLane{lane},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-admission terminal cancellation error=%v", err)
	}
}

type emptyErrorTree struct{}

func (emptyErrorTree) Error() string   { return "empty error tree" }
func (emptyErrorTree) Unwrap() []error { return nil }

func TestTerminalErrorClassificationNeverHidesHardFailures(t *testing.T) {
	allowed := []error{protocolsession.ErrWriterStopped, context.Canceled}
	if !errorTreeContainsOnly(nil, allowed...) {
		t.Fatal("nil terminal error was classified as hard")
	}
	expectedOnly := errors.Join(
		fmtWrappedError{cause: protocolsession.ErrWriterStopped},
		context.Canceled,
	)
	if !errorTreeContainsOnly(expectedOnly, allowed...) {
		t.Fatalf("expected shutdown tree was classified as hard: %v", expectedOnly)
	}
	hardFailure := errors.New("terminal signature preparation failed")
	if errorTreeContainsOnly(errors.Join(expectedOnly, hardFailure), allowed...) {
		t.Fatal("joined hard terminal failure was hidden")
	}
	if errorTreeContainsOnly(emptyErrorTree{}, allowed...) {
		t.Fatal("empty multi-error tree was treated as successful shutdown")
	}
	if errorTreeContainsOnly(hardFailure, allowed...) {
		t.Fatal("hard terminal leaf was classified as expected shutdown")
	}
}

type fmtWrappedError struct{ cause error }

func (err fmtWrappedError) Error() string { return "wrapped: " + err.cause.Error() }
func (err fmtWrappedError) Unwrap() error { return err.cause }
