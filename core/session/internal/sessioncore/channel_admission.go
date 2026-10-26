package sessioncore

import (
	"context"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session"
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
			return nil, session.ErrHandshake
		}
		return append([]byte(nil), frame...), nil
	}
}
