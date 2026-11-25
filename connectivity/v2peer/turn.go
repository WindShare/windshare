package v2peer

import (
	"context"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/pionpeer"
	"github.com/windshare/windshare/connectivity/turnsession"
	"github.com/windshare/windshare/connectivity/v2signal"
	p "github.com/windshare/windshare/core/session/protocolsession"
)

type turnPeerConnection interface {
	PeerConnection
	ReceiverPeerConnection
}

type turnPeerFactory func(context.Context, nativepeer.AttemptRequest, p.TURNGrant) (turnPeerConnection, error)

func openTURNPeer(ctx context.Context, request nativepeer.AttemptRequest, g p.TURNGrant) (turnPeerConnection, error) {
	relay, err := iceconfig.NewTURNRelay(g.URLs, g.Username, g.Password)
	if err != nil {
		return nil, err
	}
	peer, err := pionpeer.NewTURNPeerConnection(ctx, request.Configuration, relay, pionpeer.AttemptConfig{
		ProtocolSessionID: request.ProtocolSessionID, PeerPathID: request.Binding.PeerPathID, AttemptID: request.Binding.AttemptID,
	})
	if err != nil {
		return nil, err
	}
	return peer, nil
}

func (a *peerAttempt) openTURN(ctx context.Context, request nativepeer.AttemptRequest, grant turnsession.OfferGrant) (PeerConnection, error) {
	release, err := a.config.factory.turnCapacity.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	a.releaseTURN = release
	g, ok := grant.GrantAt(a.config.factory.now())
	if !ok {
		return nil, turnsession.ErrUnknownGrant
	}
	return a.config.factory.turnPeers(ctx, request, g)
}
func (f *ReceiverFactory) StartTURNBinding(ctx context.Context, signaling ReceiverSignaling, lanes ReceiverLaneSession, binding v2signal.Binding, g p.TURNGrant) (*ReceiverAttempt, error) {
	if f == nil || binding.Purpose != v2signal.TURNRelay || !g.Valid() {
		return nil, ErrConfig
	}
	return f.startBinding(ctx, signaling, lanes, binding, nil, &g)
}
