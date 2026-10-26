package sessioncore

import (
	"context"
	"crypto/ed25519"
	"errors"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestTerminalFanoutPreservesCallerCancellationBeforeAdmission(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	lane, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	stopWriterBeforeAdmission(t, lane.writer)
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: sessioncontract.SessionStoppedCode, Message: "stop",
	})
	if err != nil {
		t.Fatal(err)
	}
	callerContext, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	outbound := Responses{
		runtime:    runtime,
		privateKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
	}
	err = outbound.sendTerminalRecipients(
		callerContext,
		body,
		[]selectedLane{lane},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-admission terminal cancellation error=%v", err)
	}
}

type emptyErrorTree struct{}

func (emptyErrorTree) Error() string { return "empty error tree" }

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
