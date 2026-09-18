package receive

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/transport/relayv2"
)

type failedOutputAuthority struct {
	mode              OutputMode
	bindErr, closeErr error
	closed            int
}

func (a *failedOutputAuthority) BindDestination(context.Context) (OutputMode, error) {
	return a.mode, a.bindErr
}
func (a *failedOutputAuthority) LookupActive(context.Context, transfer.SelectionSpec) (OutputLookup, error) {
	panic("failed preparation performed selection")
}
func (a *failedOutputAuthority) Close() error { a.closed++; return a.closeErr }

func TestReceivePreparationRetainsCleanupFailureAndJoinsBeforeReturn(t *testing.T) {
	bindErr := errors.New("target revoked")
	closeErr := errors.New("output close failed")
	for _, test := range []struct {
		name       string
		factoryErr error
		mode       OutputMode
		bindErr    error
		want       error
	}{
		{"binding failure", nil, OutputResumable, bindErr, bindErr},
		{"construction rollback", bindErr, OutputResumable, nil, bindErr},
		{"invalid output capability", nil, 0, nil, errGetOutputAdapterContract},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := &failedOutputAuthority{mode: test.mode, bindErr: test.bindErr, closeErr: closeErr}
			request := Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly, Output: OutputFactoryFunc(func(OutputConfig) (OutputAuthority, error) { return output, test.factoryErr }), Destination: t.TempDir(), Diagnostics: true}
			dialed := false
			result := Run(context.Background(), request, Dependencies{ReceiverDial: func(context.Context, relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error) {
				dialed = true
				return nil, nil
			}})
			if dialed || output.closed != 1 || !errors.Is(result.Err, test.want) || !errors.Is(result.CleanupError, closeErr) || result.Outcome != task.OutcomeFailed {
				t.Fatalf("closed=%d dialed=%v result=%+v", output.closed, dialed, result)
			}
		})
	}
}
func TestReceiveCancellationCannotHideOutputCleanupFailure(t *testing.T) {
	cleanup := errors.New("output cleanup failed")
	output := &failedOutputAuthority{mode: OutputResumable, closeErr: cleanup}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := Run(ctx, Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly, Output: OutputFactoryFunc(func(OutputConfig) (OutputAuthority, error) { return output, nil }), Destination: t.TempDir()}, Dependencies{})
	if !errors.Is(result.Err, context.Canceled) || !errors.Is(result.CleanupError, cleanup) || output.closed != 1 || result.Outcome != task.OutcomeFailed {
		t.Fatalf("closed=%d result=%+v", output.closed, result)
	}
}
func TestReceiveRejectsMissingAndMalformedOutputFactories(t *testing.T) {
	for _, factory := range []OutputFactory{nil, OutputFactoryFunc(nil), OutputFactoryFunc(func(OutputConfig) (OutputAuthority, error) { return nil, nil })} {
		result := Run(context.Background(), Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly, Output: factory, Destination: t.TempDir()}, Dependencies{})
		if result.Outcome != task.OutcomeFailed || result.Err == nil {
			t.Fatalf("factory=%T result=%+v", factory, result)
		}
	}
}
func TestReceiveCleanupFailureRevokesNominalSuccessAndCancellation(t *testing.T) {
	cleanup := errors.New("authority close failed")
	for _, canceled := range []bool{false, true} {
		job := successfulJobResult(t)
		if canceled {
			job.Outcome = transfer.DirectTreeOutcomePaused
			job.TerminationInterruption = transfer.TransferInterruptionCanceled
			job.TerminationCause = context.Canceled
		}
		result, err := Settle(SettlementInput{Result: job, ContextError: context.Canceled, CleanupError: cleanup})
		failure, classified := errors.AsType[Failure](result.Err)
		if err != nil || result.Outcome != task.OutcomeFailed || result.FailureClass != task.FailureLocal || !classified || !errors.Is(failure.Cause, cleanup) {
			t.Fatalf("canceled=%v result=%+v err=%v", canceled, result, err)
		}
	}
}
