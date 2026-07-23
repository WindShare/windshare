package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func TestShareLifecycleSettlementAcceptsOnlyBenignComponents(t *testing.T) {
	ledger := newShareTerminalLedger()
	naturalSession := testShareSessionID(t, 1)
	deliveredSession := testShareSessionID(t, 2)
	ledger.ObserveSenderTerminal(sessionruntime.SenderTerminalObservation{
		ProtocolSessionID:    naturalSession,
		TransportDisposition: sessionruntime.SenderTerminalTransportRetired,
		Outcome:              sessionruntime.SenderTerminalOutcomeDropped,
		Decision:             sessionruntime.SenderTerminalDecisionNaturalRetirement,
	})
	ledger.ObserveSenderTerminal(sessionruntime.SenderTerminalObservation{
		ProtocolSessionID:    deliveredSession,
		TransportDisposition: sessionruntime.SenderTerminalTransportAccepted,
		Outcome:              sessionruntime.SenderTerminalOutcomeUnknown,
		Decision:             sessionruntime.SenderTerminalDecisionFailed,
	})
	ledger.ObserveSenderTerminal(sessionruntime.SenderTerminalObservation{
		ProtocolSessionID:    deliveredSession,
		TransportDisposition: sessionruntime.SenderTerminalTransportAccepted,
		Outcome:              sessionruntime.SenderTerminalOutcomeDelivered,
		Decision:             sessionruntime.SenderTerminalDecisionDelivered,
	})

	settlement := settleShareLifecycle(
		shareShutdownCallerInterrupted,
		context.Canceled,
		errors.Join(
			fmt.Errorf("accept interrupted: %w", context.Canceled),
			fmt.Errorf("relay lifecycle interrupted: %w", errSenderRelayRecoveryStopped),
		),
		nil,
		ledger.Snapshot(),
	)
	if err := settlement.Err(); err != nil {
		t.Fatalf("benign settlement error=%v", err)
	}
	if settlement.serve.outcome != shareComponentInterrupted ||
		settlement.stop.outcome != shareComponentCompleted ||
		settlement.decision != shareSettlementClean {
		t.Fatalf("benign settlement=%+v", settlement)
	}
	if settlement.terminals.sessions != 2 || settlement.terminals.deliveredSessions != 1 ||
		settlement.terminals.naturallyRetiredSessions != 1 || settlement.terminals.failedSessions != 0 ||
		settlement.terminals.acceptedFailedLanes != 1 {
		t.Fatalf("terminal summary=%+v", settlement.terminals)
	}
}

func TestShareLifecycleSettlementPreservesMixedJoinedStopFailure(t *testing.T) {
	stopFailure := errors.New("accepted terminal transport failed")
	settlement := settleShareLifecycle(
		shareShutdownCallerInterrupted,
		context.Canceled,
		context.Canceled,
		errors.Join(context.Canceled, stopFailure),
		shareTerminalSummary{},
	)
	if settlement.stop.outcome != shareComponentFailed || settlement.decision != shareSettlementFailed {
		t.Fatalf("mixed stop settlement=%+v", settlement)
	}
	if err := settlement.Err(); !errors.Is(err, context.Canceled) || !errors.Is(err, stopFailure) {
		t.Fatalf("mixed stop failure=%v", err)
	}
}

func TestShareLifecycleSettlementPreservesAcceptedCancellationFailure(t *testing.T) {
	ledger := newShareTerminalLedger()
	ledger.ObserveSenderTerminal(sessionruntime.SenderTerminalObservation{
		ProtocolSessionID:    testShareSessionID(t, 3),
		TransportDisposition: sessionruntime.SenderTerminalTransportAccepted,
		Outcome:              sessionruntime.SenderTerminalOutcomeUnknown,
		Decision:             sessionruntime.SenderTerminalDecisionFailed,
	})
	settlement := settleShareLifecycle(
		shareShutdownCallerInterrupted,
		context.Canceled,
		context.Canceled,
		context.Canceled,
		ledger.Snapshot(),
	)
	if settlement.stop.outcome != shareComponentFailed || settlement.decision != shareSettlementFailed {
		t.Fatalf("accepted cancellation settlement=%+v", settlement)
	}
	if err := settlement.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("accepted cancellation failure=%v", err)
	}
	if settlement.terminals.failedSessions != 1 || settlement.terminals.acceptedFailedLanes != 1 {
		t.Fatalf("accepted cancellation summary=%+v", settlement.terminals)
	}

	observedFailure := settleShareLifecycle(
		shareShutdownCallerInterrupted,
		context.Canceled,
		context.Canceled,
		nil,
		ledger.Snapshot(),
	)
	if err := observedFailure.Err(); !errors.Is(err, errShareTerminalDecisionFailed) {
		t.Fatalf("failed terminal decision without stop error=%v", err)
	}
}

func TestShareLifecycleSettlementPreservesDurableStopCancellation(t *testing.T) {
	settlement := settleShareLifecycle(
		shareShutdownCallerInterrupted,
		context.Canceled,
		context.Canceled,
		context.Canceled,
		shareTerminalSummary{},
	)
	if settlement.stop.outcome != shareComponentFailed || settlement.decision != shareSettlementFailed {
		t.Fatalf("durable stop cancellation settlement=%+v", settlement)
	}
	if err := settlement.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("durable stop cancellation=%v", err)
	}
}

func TestShareLifecycleSettlementKeepsAlreadyEndedRuntimeBenign(t *testing.T) {
	settlement := settleShareLifecycle(
		shareShutdownCallerInterrupted,
		context.Canceled,
		context.Canceled,
		nil,
		shareTerminalSummary{},
	)
	if err := settlement.Err(); err != nil {
		t.Fatalf("already-ended runtime settlement error=%v", err)
	}
	if settlement.stop.outcome != shareComponentCompleted || settlement.decision != shareSettlementClean {
		t.Fatalf("already-ended runtime settlement=%+v", settlement)
	}
}

func TestInterruptedShareSettlementRetainsServeDoneFailure(t *testing.T) {
	serveFailure := errors.New("relay accept lifecycle failed")
	serveDone := make(chan error, 1)
	serveDone <- serveFailure
	serveErr := awaitInterruptedShareServe(serveDone, time.Second)
	settlement := settleShareLifecycle(
		shareShutdownCallerInterrupted,
		context.Canceled,
		serveErr,
		nil,
		shareTerminalSummary{},
	)
	if settlement.serve.outcome != shareComponentFailed || settlement.decision != shareSettlementFailed {
		t.Fatalf("serve failure settlement=%+v", settlement)
	}
	if err := settlement.Err(); !errors.Is(err, serveFailure) {
		t.Fatalf("serve failure=%v", err)
	}
	if trigger := shareTriggerAfterServe(context.Canceled, serveFailure); trigger != shareShutdownServeEnded {
		t.Fatalf("non-cancellation serve trigger=%s", trigger)
	}
	if trigger := shareTriggerAfterServe(context.Canceled, context.Canceled); trigger != shareShutdownCallerInterrupted {
		t.Fatalf("cancellation serve trigger=%s", trigger)
	}
	advertised := settleShareLifecycle(
		shareShutdownCallerInterrupted,
		context.Canceled,
		advertisedShareCancellation{},
		nil,
		shareTerminalSummary{},
	)
	if advertised.serve.outcome != shareComponentFailed || !errors.Is(advertised.Err(), context.Canceled) {
		t.Fatalf("advertised cancellation settlement=%+v error=%v", advertised, advertised.Err())
	}

	if err := awaitInterruptedShareServe(make(chan error), 0); !errors.Is(err, errShareServeJoinTimedOut) {
		t.Fatalf("unjoined serve result=%v", err)
	}
}

type advertisedShareCancellation struct{}

func (advertisedShareCancellation) Error() string { return "advertised cancellation" }
func (advertisedShareCancellation) Is(target error) bool {
	return target == context.Canceled
}

func testShareSessionID(t *testing.T, marker byte) protocolsession.ProtocolSessionID {
	t.Helper()
	raw := make([]byte, protocolsession.IdentityBytes)
	raw[len(raw)-1] = marker
	id, err := protocolsession.ProtocolSessionIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
