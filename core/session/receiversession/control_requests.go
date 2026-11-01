package receiversession

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// Each physical attempt owns a new operation identity. Retire the old sink and
// cancellation authority before restarting a repeatable control transaction;
// replaying its ID could hit the sender's completed-operation tombstone.
func runControlRequest[T any](
	ctx context.Context,
	rpc requestClient,
	kind protocolsession.MessageKind,
	body []byte,
	receive func(*sessioncore.Call) (T, error),
) (T, error) {
	var zero T
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		call, err := rpc.Begin(ctx, kind, body)
		if err != nil {
			return zero, err
		}
		value, receiveErr := receive(call)
		cleanupErr := rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort)
		if !errors.Is(receiveErr, sessioncore.ErrRequestCarrierRetired) {
			return value, receiveErr
		}
		if cleanupErr != nil {
			return zero, errors.Join(receiveErr, cleanupErr)
		}
		// The request's carrier is gone; only another usable lane in this same
		// session can recover the exchange without resetting transfer progress.
		if !rpc.Runtime().HasUsableLanes() {
			return zero, receiveErr
		}
	}
}

type controlResult struct {
	message     protocolsession.Message
	operationID protocolsession.OperationID
}

func requestControlResult(ctx context.Context, rpc requestClient, kind protocolsession.MessageKind, body []byte) (controlResult, error) {
	return runControlRequest(ctx, rpc, kind, body, func(call *sessioncore.Call) (controlResult, error) {
		message, err := rpc.Await(ctx, call)
		return controlResult{message: message, operationID: call.ID()}, err
	})
}
