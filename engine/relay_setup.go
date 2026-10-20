package engine

import (
	"context"
	"errors"

	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/engine/relaysetup"
)

type RelaySetupRequest = relaysetup.Request
type RelaySetupResult = relaysetup.Result
type RelaySetupProgress = relaysetup.Progress

func (engine *Engine) StartRelaySetup(ctx context.Context, request RelaySetupRequest) (*Task[RelaySetupResult], error) {
	return start(engine, ctx, func(ctx context.Context, control task.Control) task.Completion[RelaySetupResult] {
		result, err := engine.relaySetup.Run(ctx, request, func(event relaysetup.Progress) { control.Emit(event) })
		return task.Completion[RelaySetupResult]{Value: result, Settlement: relaySetupSettlement(ctx, err)}
	})
}

func (engine *Engine) CancelRelaySetup(ctx context.Context, address string) error {
	// Cancellation changes durable local and remote state, so shutdown must join
	// it just as it joins enrollment instead of admitting work after Close.
	current, err := start(engine, ctx, func(ctx context.Context, _ task.Control) task.Completion[struct{}] {
		return task.Completion[struct{}]{Settlement: relaySetupSettlement(ctx, engine.relaySetup.Cancel(ctx, address))}
	})
	if err != nil {
		return err
	}
	result, err := current.Wait(ctx)
	return errors.Join(err, result.Err)
}

func relaySetupSettlement(ctx context.Context, err error) task.Settlement {
	if err == nil {
		return task.Settlement{Outcome: task.OutcomeSuccess}
	}
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return task.Settlement{Outcome: task.OutcomeCancelled, Err: err}
	}
	return task.Settlement{Outcome: task.OutcomeFailed, FailureClass: task.FailureLocal, Err: err}
}
