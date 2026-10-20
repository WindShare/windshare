package peerset

import (
	"context"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

type ReceiverConfig struct {
	Routes        ReceiverRoutes
	Factory       *v2peer.ReceiverFactory
	Signaling     v2peer.ReceiverSignaling
	Lanes         v2peer.ReceiverLaneSession
	Demand        Demand
	StopAfterWave bool
}

type ReceiverRoutes uint8

const (
	DirectRoutes ReceiverRoutes = iota
	UserRelayRoutes
)

func OpenReceiver(ctx context.Context, config Config, receiver ReceiverConfig) (*Path, error) {
	if receiver.Factory == nil {
		return nil, ErrConfig
	}
	owner, err := New(config)
	if err != nil {
		return nil, err
	}
	return owner.openDirect(ctx, receiver)
}

func (owner *PeerSet) openDirect(ctx context.Context, receiver ReceiverConfig) (*Path, error) {
	var sessionID protocolsession.ProtocolSessionID
	if session, ok := receiver.Lanes.(interface {
		ProtocolSessionID() protocolsession.ProtocolSessionID
	}); ok {
		sessionID = session.ProtocolSessionID()
	}
	var controls sessionruntime.PeerPathControlSession
	if session, ok := receiver.Lanes.(sessionruntime.PeerPathControlSession); ok {
		controls = session
	}
	pathConfig := PathConfig{SessionID: sessionID, Demand: receiver.Demand, StopAfterWave: receiver.StopAfterWave,
		Native: receiver.Factory.NativeConnectivity(), Controls: controls,
		Start: func(ctx context.Context, binding v2signal.Binding) (Attempt, error) {
			return receiver.Factory.StartBinding(ctx, receiver.Signaling, receiver.Lanes, binding)
		},
	}
	if receiver.Factory.NativeConnectivity() != nil {
		pathConfig.Prepare = func(ctx context.Context, binding v2signal.Binding) (PreparedStarter, error) {
			prepared, err := receiver.Factory.PrepareBinding(ctx, receiver.Lanes, binding)
			if err != nil {
				return PreparedStarter{}, err
			}
			return PreparedStarter{Close: prepared.Close, Start: func(ctx context.Context, binding v2signal.Binding) (Attempt, error) {
				return receiver.Factory.StartPreparedBinding(ctx, receiver.Signaling, receiver.Lanes, binding, prepared)
			}}, nil
		}
	}
	return owner.Open(ctx, pathConfig)
}
