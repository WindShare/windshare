package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestShareLifecycleSettlementAcceptsOnlyBenignComponents(t *testing.T) {
	settlement := settleShareLifecycle(
		shareShutdownCallerInterrupted,
		context.Canceled,
		errors.Join(
			fmt.Errorf("accept interrupted: %w", context.Canceled),
			fmt.Errorf("relay lifecycle interrupted: %w", errSenderRelayRecoveryStopped),
		),
		nil,
	)
	if err := settlement.Err(); err != nil {
		t.Fatalf("benign settlement error=%v", err)
	}
	if settlement.serve.outcome != shareComponentInterrupted ||
		settlement.stop.outcome != shareComponentCompleted ||
		settlement.decision != shareSettlementClean {
		t.Fatalf("benign settlement=%+v", settlement)
	}
}

func TestShareLifecycleSettlementPreservesMixedJoinedStopFailure(t *testing.T) {
	stopFailure := errors.New("accepted terminal transport failed")
	settlement := settleShareLifecycle(
		shareShutdownCallerInterrupted,
		context.Canceled,
		context.Canceled,
		errors.Join(context.Canceled, stopFailure),
	)
	if settlement.stop.outcome != shareComponentFailed || settlement.decision != shareSettlementFailed {
		t.Fatalf("mixed stop settlement=%+v", settlement)
	}
	if err := settlement.Err(); !errors.Is(err, context.Canceled) || !errors.Is(err, stopFailure) {
		t.Fatalf("mixed stop failure=%v", err)
	}
}

func TestShareLifecycleSettlementPreservesDurableStopCancellation(t *testing.T) {
	settlement := settleShareLifecycle(
		shareShutdownCallerInterrupted,
		context.Canceled,
		context.Canceled,
		context.Canceled,
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
