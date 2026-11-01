package receive

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"testing"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/diagnostics"
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
func (a *failedOutputAuthority) SelectOperation(context.Context, transfer.SelectionSpec, receivecontract.OperationSelection) (OutputLookup, error) {
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
			request := Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly, Output: OutputFactoryFunc(func(OutputConfig) (OutputAuthority, error) { return output, test.factoryErr }), Destination: t.TempDir(), Diagnostics: testDiagnosticSink(t)}
			dialed := false
			result := Run(context.Background(), request, Dependencies{ReceiverDial: func(context.Context, relayv2.ReceiverConfig) (relayset.ReceiverConnection, error) {
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

func testDiagnosticSink(t *testing.T) diagnostics.Sink {
	t.Helper()
	stream, err := diagnostics.NewStream(256, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	return stream.Sink()
}

func TestReceiveRejectsNonCanonicalSelectionAsInvalidInputBeforeIO(t *testing.T) {
	for _, test := range []struct {
		name string
		only []string
	}{
		{"trailing slash", []string{"tree/"}},
		{"dot prefix", []string{"./tree"}},
		{"empty segment", []string{"tree//file.bin"}},
		{"absolute", []string{"/tree"}},
		{"canonical alias", []string{"tree", "tree/"}},
		{"non-normalized Unicode", []string{"cafe\u0301"}},
		{"duplicate", []string{"tree", "tree"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			created, dialed := false, false
			request := Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly, Only: test.only,
				Output: OutputFactoryFunc(func(OutputConfig) (OutputAuthority, error) {
					created = true
					return &failedOutputAuthority{mode: OutputResumable}, nil
				}), Destination: t.TempDir(), Diagnostics: testDiagnosticSink(t)}
			result := Run(context.Background(), request, Dependencies{ReceiverDial: func(context.Context, relayv2.ReceiverConfig) (relayset.ReceiverConnection, error) {
				dialed = true
				return nil, errors.New("invalid selection reached the relay")
			}})
			failure, classified := errors.AsType[Failure](result.Err)
			if created || dialed || !result.Value.Connectivity.Final || result.Outcome != task.OutcomeFailed || result.FailureClass != task.FailureUsage || !classified ||
				failure.Code != FailureInvalidInput || !errors.Is(result.Err, transfer.ErrInvalidSelectionRules) || result.Value.Recovery.Reason != RecoveryInvalidRequest {
				t.Fatalf("only=%q created=%v dialed=%v outcome=%d class=%d code=%d reason=%d err=%v", test.only, created, dialed,
					result.Outcome, result.FailureClass, failure.Code, result.Value.Recovery.Reason, result.Err)
			}
		})
	}
}
