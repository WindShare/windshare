package sessioncore

import (
	"context"
	"github.com/windshare/windshare/core/framechannel"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// Some transports reserve provisional resources before E2E authentication.
// Only the authenticator can authorize retaining them; byte traffic is no proof.
type channelAdmissionConfirmer interface {
	ConfirmAdmission(context.Context) error
}

func ConfirmChannelAdmission(ctx context.Context, channel protocolsession.FrameChannel) error {
	if admission, ok := channel.(channelAdmissionConfirmer); ok {
		return admission.ConfirmAdmission(ctx)
	}
	return nil
}

type HandedOffChannel struct {
	protocolsession.FrameChannel
	receive <-chan framechannel.Frame
}

func (channel *HandedOffChannel) Recv() <-chan framechannel.Frame { return channel.receive }

func ReceiveHandshake(ctx context.Context, receive <-chan framechannel.Frame) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case frame, ok := <-receive:
		if !ok || len(frame) == 0 {
			return nil, sessioncontract.ErrHandshake
		}
		return append([]byte(nil), frame...), nil
	}
}

func (outbound Responses) SendFragment(ctx context.Context, message protocolsession.Message) error {
	operationID, ok := message.OperationID()
	if !ok {
		return sessioncontract.ErrOperationMissing
	}
	_, err := outbound.executeResponse(ctx, message.Kind(), operationID, sessionobs.ProtocolErrorContent{}, func(transaction *outboundTransaction) (outboundLaneAttempt, error) {
		return func(lane selectedLane, permit protocolsession.OutboundReplayPermit) (protocolsession.SendReceipt, error) {
			if !permit.IsZero() {
				return lane.writer.TryDataReplay(message, permit)
			}
			return lane.writer.TryAuthorizedData(message, transaction.authority)
		}, nil
	})
	return err
}

func (outbound Responses) SendOperationError(
	ctx context.Context,
	operationID protocolsession.OperationID,
	failure contentflow.OperationFailure,
) error {
	body, err := protocolsession.EncodeOperationFailure(failure)
	if err != nil {
		_, sendErr := outbound.executeResponse(ctx, protocolsession.MessageOperationError, operationID, sessionobs.ProtocolErrorContent{}, func(*outboundTransaction) (outboundLaneAttempt, error) { return nil, err })
		return sendErr
	}
	_, err = outbound.SendControl(ctx, protocolsession.MessageOperationError, operationID, body)
	return err
}
