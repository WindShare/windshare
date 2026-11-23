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

func openTURNPeer(request nativepeer.AttemptRequest, g p.TURNGrant) (*pionpeer.Connection, error) {
	relay, err := iceconfig.NewTURNRelay(g.URLs, g.Username, g.Password)
	if err != nil {
		return nil, err
	}
	return pionpeer.NewTURNPeerConnection(request.Configuration, relay, pionpeer.AttemptConfig{
		ProtocolSessionID: request.ProtocolSessionID, PeerPathID: request.Binding.PeerPathID, AttemptID: request.Binding.AttemptID,
	})
}
func (a *peerAttempt) openTURN(ctx context.Context, request nativepeer.AttemptRequest) (PeerConnection, error) {
	release, err := a.config.factory.turnCapacity.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	a.releaseTURN = release
	g, ok := a.config.turn.Lookup(a.config.offer.TURNGrantID)
	if !ok {
		return nil, turnsession.ErrUnknownGrant
	}
	peer, err := openTURNPeer(request, g)
	if err != nil {
		return nil, err
	}
	return peer, nil
}
func (f *ReceiverFactory) StartTURNBinding(ctx context.Context, signaling ReceiverSignaling, lanes ReceiverLaneSession, binding v2signal.Binding, g p.TURNGrant) (*ReceiverAttempt, error) {
	if f == nil || binding.Purpose != v2signal.TURNRelay || !g.Valid() {
		return nil, ErrConfig
	}
	return f.startBinding(ctx, signaling, lanes, binding, nil, &g)
}
