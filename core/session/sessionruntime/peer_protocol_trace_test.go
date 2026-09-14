package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func newReceiverPeerProtocolTraceFixture(t *testing.T) (receiverPeerTerminalFixture, *protocolTraceRecorder) {
	t.Helper()
	runtime, _ := newUnstartedRuntimeWithContinuations(t, protocolsession.RoleReceiver,
		protocolsession.OperationLimits{}, nil, continuationReplayClassifier{})
	recorder := newProtocolTraceRecorder(runtime)
	return openReceiverPeerTerminalFixture(t, runtime, 0xc8), recorder
}

func assertReceiverPeerProtocolTrace(t *testing.T, fixture receiverPeerTerminalFixture, recorder *protocolTraceRecorder, stage ProtocolOperationStage, cause ProtocolOperationCause) {
	t.Helper()
	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("terminal observations = %+v, want exactly one", events)
	}
	event := events[0]
	if event.Stage != stage || event.Cause != cause ||
		event.OperationID != fixture.operation.OperationID() ||
		event.ProtocolSessionID != fixture.runtime.sessionID ||
		event.RequestKind != protocolsession.MessagePeerOffer ||
		!event.HasSend || !event.SendSettled || !event.SendAdmitted ||
		event.SendOutcome != protocolsession.SendOutcomeTransportConfirmed {
		t.Fatalf("terminal observation = %+v, want stage %v cause %v", event, stage, cause)
	}
}

func TestReceiverPeerProtocolTraceNormalCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, recorder := newReceiverPeerProtocolTraceFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		received := make(chan ReceiverPeerReceiveResult, 1)
		go func() { received <- fixture.operation.Receive(ctx) }()
		synctest.Wait()
		cancel()
		terminal := requireReceiverPeerTermination(t, <-received)
		if terminal.TransitionProvenance() != ReceiverPeerProvenanceLocalContextEnded ||
			!receiverPeerDiagnosticsContain(terminal.Diagnostics(), ReceiverPeerDiagnosticContextCanceled) {
			t.Fatalf("local cancellation lost its owner or diagnostics: %+v", terminal)
		}
		_ = fixture.operation.Terminate(context.Background())
		fixture.operation.rpc.Close()
		assertReceiverPeerProtocolTrace(t, fixture, recorder, ProtocolOperationReceiverEnded, ProtocolOperationCauseCanceled)
	})
}

func TestReceiverPeerProtocolTraceExplicitStopWakesReceive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, recorder := newReceiverPeerProtocolTraceFixture(t)
		received := make(chan ReceiverPeerReceiveResult, 1)
		go func() { received <- fixture.operation.Receive(context.Background()) }()
		synctest.Wait()
		terminal := fixture.operation.Terminate(context.Background())
		requireReceiverPeerTermination(t, <-received)
		if terminal.TransitionProvenance() != ReceiverPeerProvenanceLocalExplicitStop {
			t.Fatalf("explicit stop lost ownership: %+v", terminal)
		}
		assertReceiverPeerProtocolTrace(t, fixture, recorder, ProtocolOperationReceiverEnded, ProtocolOperationCauseOperationClosed)
	})
}

func TestReceiverPeerProtocolTraceWaitsForJoinedFault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, recorder := newReceiverPeerProtocolTraceFixture(t)
		call, terminal, err := fixture.operation.beginReceive()
		if err != nil || terminal != nil {
			t.Fatalf("begin receive: %v %+v", err, terminal)
		}
		terminated := make(chan ReceiverPeerTermination, 1)
		go func() { terminated <- fixture.operation.Terminate(context.Background()) }()
		synctest.Wait()
		// Receive may already hold an authenticated response when local cleanup
		// closes its RPC sink. Only the joined owner can classify the final result.
		if events := recorder.snapshot(); len(events) != 0 {
			t.Errorf("RPC cleanup published before the in-flight receive joined: %+v", events)
		}
		result := fixture.operation.terminateFromReceive(call, newReceiverPeerTerminalEvidence(
			ReceiverPeerTerminalAuthorityRemote, ReceiverPeerProvenanceRemoteUnknownControl,
			ReceiverPeerTerminalOperationOnly, receiverPeerDiagnostic(ReceiverPeerDiagnosticUnknownControl)))
		joined := requireReceiverPeerTermination(t, result)
		if joined.Authority() != ReceiverPeerTerminalAuthorityLocal ||
			!receiverPeerDiagnosticsContain(joined.Diagnostics(), ReceiverPeerDiagnosticUnknownControl) {
			t.Fatalf("losing remote fault was not retained: %+v", joined)
		}
		<-terminated
		assertReceiverPeerProtocolTrace(t, fixture, recorder, ProtocolOperationReceiverFailed, ProtocolOperationCauseProtocolFailure)
	})
}

func TestReceiverPeerProtocolTraceRetainsTerminalFailures(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		stop  func(*testing.T, receiverPeerTerminalFixture)
		cause ProtocolOperationCause
	}{
		{"deadline", func(t *testing.T, fixture receiverPeerTerminalFixture) {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now())
			defer cancel()
			requireReceiverPeerTermination(t, fixture.operation.Receive(ctx))
		}, ProtocolOperationCauseDeadline},
		{"remote control failure", func(t *testing.T, fixture receiverPeerTerminalFixture) {
			enqueueUnexpectedPeerResponse(t, fixture.call)
			requireReceiverPeerTermination(t, fixture.operation.Receive(context.Background()))
		}, ProtocolOperationCauseProtocolFailure},
		{"runtime shutdown", func(_ *testing.T, fixture receiverPeerTerminalFixture) {
			fixture.runtime.cancel()
			fixture.operation.rpc.Close()
			_ = fixture.operation.Terminate(context.Background())
		}, ProtocolOperationCauseRuntimeClosed},
		{"cancellation with cleanup failure", func(t *testing.T, fixture receiverPeerTerminalFixture) {
			result := fixture.operation.terminateWithoutReceive(
				receiverPeerLocalEvidence(ReceiverPeerProvenanceLocalContextEnded, context.Canceled),
				func(call *operationCall) error {
					err := fixture.operation.rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort)
					return errors.Join(err, context.Canceled, errors.New("cleanup failed"))
				})
			requireReceiverPeerTermination(t, result)
		}, ProtocolOperationCauseProtocolFailure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture, recorder := newReceiverPeerProtocolTraceFixture(t)
				testCase.stop(t, fixture)
				assertReceiverPeerProtocolTrace(t, fixture, recorder, ProtocolOperationReceiverFailed, testCase.cause)
			})
		})
	}
}

func TestProtocolTraceDoesNotTreatUnownedCancellationAsNormal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, recorder := newReceiverPeerProtocolTraceFixture(t)
		call, err := fixture.operation.rpc.begin(context.Background(), protocolsession.MessageListChildren, []byte{0xa0})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := fixture.operation.rpc.await(ctx, call); !errors.Is(err, context.Canceled) {
			t.Fatalf("request cancellation = %v", err)
		}
		fixture.operation.rpc.end(call)
		events := recorder.snapshot()
		if len(events) != 1 || events[0].OperationID != call.id ||
			events[0].Stage != ProtocolOperationReceiverFailed || events[0].Cause != ProtocolOperationCauseCanceled {
			t.Fatalf("cancellation without a normal terminal owner was hidden: %+v", events)
		}
	})
}
