package sessionruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestOutboundTransactionRequiresExactLiveGenerationAuthority(t *testing.T) {
	t.Run("missing operation permit", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
		operationID := id16[protocolsession.OperationID](131)
		route := runtime.routes.reserve(operationID, runtime.initial)
		if route == nil {
			t.Fatal("reserve operation route")
		}
		ctx := bindOutboundRoute(context.Background(), operationID, route)

		if _, err := beginOutboundTransaction(runtime, ctx, operationID); !errors.Is(err, ErrOperationMissing) {
			t.Fatalf("permit-less transaction error=%v", err)
		}
		if runtime.routes.current(operationID) != route {
			t.Fatal("rejected transaction consumed route authority")
		}
	})

	t.Run("missing generation", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
		operationID := id16[protocolsession.OperationID](132)
		request, err := protocolsession.NewMessage(
			protocolsession.MessageRequestBlocks,
			&operationID,
			[]byte{0xa1, 0x00, 0x01},
		)
		if err != nil {
			t.Fatal(err)
		}
		fullContext, route := testOutboundOperationContext(t, runtime, runtime.initial, request)
		permit, ok := protocolsession.OutboundOperationPermitFromContext(fullContext, operationID)
		if !ok {
			t.Fatal("fixture did not expose operation permit")
		}
		ctx := bindOutboundRoute(context.Background(), operationID, route)
		ctx = protocolsession.WithOutboundOperationPermit(ctx, permit)

		if _, err := beginOutboundTransaction(runtime, ctx, operationID); !errors.Is(err, ErrOperationMissing) {
			t.Fatalf("generation-less transaction error=%v", err)
		}
		if runtime.operations.ActiveCount() != 1 || runtime.routes.current(operationID) != route {
			t.Fatal("generation-less transaction mutated live operation authority")
		}
	})

	t.Run("expired generation", func(t *testing.T) {
		var nanos atomic.Int64
		nanos.Store(time.Unix(30_000, 0).UnixNano())
		now := func() time.Time { return time.Unix(0, nanos.Load()) }
		runtime, _ := newUnstartedRuntimeWithPolicy(
			t,
			protocolsession.RoleSender,
			protocolsession.OperationLimits{MaxActive: 2, MaxTombstones: 2},
			now,
		)
		operationID := id16[protocolsession.OperationID](133)
		request, err := protocolsession.NewMessage(
			protocolsession.MessageRequestBlocks,
			&operationID,
			[]byte{0xa1, 0x00, 0x01},
		)
		if err != nil {
			t.Fatal(err)
		}
		ctx, route := testOutboundOperationContext(t, runtime, runtime.initial, request)
		generation, ok := protocolsession.OperationGenerationFromContext(ctx, operationID)
		if !ok {
			t.Fatal("fixture did not expose operation generation")
		}
		if err := runtime.operations.CancelGeneration(generation); err != nil {
			t.Fatal(err)
		}
		nanos.Add((protocolsession.OperationTombstoneLifetime + time.Nanosecond).Nanoseconds())
		if runtime.operations.TombstoneCount() != 0 {
			t.Fatal("expired generation retained a tombstone")
		}

		if _, err := beginOutboundTransaction(runtime, ctx, operationID); !errors.Is(err, protocolsession.ErrUnknownOperation) {
			t.Fatalf("expired-generation transaction error=%v", err)
		}
		// Reading the route under the send lock proves failed lease acquisition
		// released serialization without weakening the route-state assertion.
		route.sendMu.Lock()
		preferredAfterFailure := route.preferred
		route.sendMu.Unlock()
		if !preferredAfterFailure.valid(true) {
			t.Fatal("failed lease acquisition erased the synchronized route")
		}
		if runtime.routes.current(operationID) != route {
			t.Fatal("stale generation consumed the route owned by cleanup")
		}
	})
}

func TestOutboundTransactionFailClosesReplayAuthorityContractViolations(t *testing.T) {
	tests := []struct {
		name       string
		completion protocolsession.SendCompletion
		attemptErr error
	}{
		{
			name:       "admitted response omitted replay permit",
			completion: protocolsession.SendCompletion{Admitted: true},
		},
		{
			name:       "writer rejected replay-less settlement",
			completion: protocolsession.SendCompletion{Settled: true},
			attemptErr: protocolsession.ErrOutboundReplayPermit,
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
			operationID := id16[protocolsession.OperationID](byte(134 + index))
			request, err := protocolsession.NewMessage(
				protocolsession.MessageRequestBlocks,
				&operationID,
				[]byte{0xa1, 0x00, 0x01},
			)
			if err != nil {
				t.Fatal(err)
			}
			ctx, _ := testOutboundOperationContext(t, runtime, runtime.initial, request)
			transaction, err := beginOutboundTransaction(runtime, ctx, operationID)
			if err != nil {
				t.Fatal(err)
			}
			authorityErr := transaction.admissionAuthorityError(
				test.completion,
				protocolsession.OutboundReplayPermit{},
				test.attemptErr,
			)
			transaction.Close()

			if !errors.Is(authorityErr, errOutboundReplayAuthority) {
				t.Fatalf("authority contract error=%v", authorityErr)
			}
			select {
			case <-runtime.ctx.Done():
			default:
				t.Fatal("missing replay authority did not fail-close the runtime")
			}
			if !runtime.operations.Terminated() || runtime.routes.len() != 0 {
				t.Fatalf("terminal=%t routes=%d", runtime.operations.Terminated(), runtime.routes.len())
			}
		})
	}
}

func TestOutboundTransactionDoesNotRetryDefinitiveOrUnsettledDrops(t *testing.T) {
	completion := protocolsession.SendCompletion{
		Settled: true,
		Outcome: protocolsession.SendOutcomeDropped,
	}
	if outcome, done := completedOutboundAttempt(completion, nil); !done || outcome != protocolsession.SendOutcomeDropped {
		t.Fatalf("definitive drop outcome=%d done=%t", outcome, done)
	}

	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	transaction := &outboundTransaction{runtime: runtime}
	physicalErr := errors.New("transport settlement is still pending")
	unsettled := protocolsession.SendCompletion{RetryableAcrossLane: true}
	if err := transaction.retryBoundaryError(context.Background(), unsettled, physicalErr); !errors.Is(err, physicalErr) || !errors.Is(err, errOutboundNotDelivered) {
		t.Fatalf("unsettled retry boundary error=%v", err)
	}
}

func TestAbandonOutboundOperationFailClosesWhenCancellationCannotBeRetained(t *testing.T) {
	runtime, _ := newUnstartedRuntimeWithPolicy(
		t,
		protocolsession.RoleSender,
		protocolsession.OperationLimits{MaxActive: 4, MaxTombstones: 1},
		nil,
	)
	targetID := id16[protocolsession.OperationID](136)
	target, err := protocolsession.NewMessage(
		protocolsession.MessageRequestBlocks,
		&targetID,
		[]byte{0xa1, 0x00, 0x01},
	)
	if err != nil {
		t.Fatal(err)
	}
	targetContext, route := testOutboundOperationContext(t, runtime, runtime.initial, target)
	targetGeneration, ok := protocolsession.OperationGenerationFromContext(targetContext, targetID)
	if !ok {
		t.Fatal("target generation missing")
	}

	fillerID := id16[protocolsession.OperationID](137)
	filler, err := protocolsession.NewMessage(
		protocolsession.MessageRequestBlocks,
		&fillerID,
		[]byte{0xa1, 0x00, 0x02},
	)
	if err != nil {
		t.Fatal(err)
	}
	fillerAdmission, err := runtime.operations.ObserveInbound(
		protocolsession.DirectionReceiverToSender,
		filler,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.operations.CancelGeneration(fillerAdmission.Generation); err != nil {
		t.Fatal(err)
	}

	abandonErr := runtime.abandonOutboundOperation(targetID, route, targetGeneration)
	if !errors.Is(abandonErr, protocolsession.ErrTombstoneBudget) {
		t.Fatalf("abandon error=%v", abandonErr)
	}
	select {
	case <-runtime.ctx.Done():
	default:
		t.Fatal("unrecordable cancellation did not fail-close the runtime")
	}
	if !runtime.operations.Terminated() || runtime.routes.len() != 0 {
		t.Fatalf("terminal=%t routes=%d", runtime.operations.Terminated(), runtime.routes.len())
	}
	if err := runtime.abandonBoundOutboundOperation(context.Background(), targetID); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("unbound abandonment error=%v", err)
	}
}
