package peerset

import (
	"context"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type ReceiverConfig struct {
	Trace         diagnostics.Sink
	Purpose       v2signal.ConnectionPurpose
	Factory       *v2peer.ReceiverFactory
	Signaling     v2peer.ReceiverSignaling
	Lanes         v2peer.ReceiverLaneSession
	Demand        Demand
	StopAfterWave bool
}

func receiverSessionID(lanes v2peer.ReceiverLaneSession) protocolsession.ProtocolSessionID {
	if session, ok := lanes.(interface {
		ProtocolSessionID() protocolsession.ProtocolSessionID
	}); ok {
		return session.ProtocolSessionID()
	}
	return protocolsession.ProtocolSessionID{}
}

func (owner *PeerSet) openDirect(ctx context.Context, receiver ReceiverConfig) (*Path, error) {
	sessionID := receiverSessionID(receiver.Lanes)
	var controls PeerPathControlSender
	if session, ok := receiver.Lanes.(PeerPathControlSender); ok {
		controls = session
	}
	pathConfig := PathConfig{Purpose: receiver.Purpose, SessionID: sessionID, Demand: receiver.Demand, StopAfterWave: receiver.StopAfterWave,
		Native: receiver.Factory.NativeConnectivity(), Controls: controls,
		attemptBudget: receiver.Factory.AttemptBudget(nativepeer.PermitLifetime),
		Start: func(ctx context.Context, binding v2signal.Binding) (Attempt, error) {
			return receiver.Factory.StartBinding(ctx, receiver.Signaling, receiver.Lanes, binding)
		},
	}
	if lanes, ok := receiver.Lanes.(ContentLaneOwner); ok {
		pathConfig.Content = lanes
	}
	if receiver.Purpose == v2signal.ThroughputExploration {
		pathConfig.Budget = NewBudget(owner.config.Clock.Now())
		pathConfig.StopAfterWave = true
	}
	if receiver.Factory.NativeConnectivity() != nil {
		pathConfig.Prepare = receiver.prepare
	}
	return owner.Open(ctx, pathConfig)
}

func (receiver ReceiverConfig) prepare(ctx context.Context, binding v2signal.Binding, queueWait time.Duration) (PreparedStarter, error) {
	prepared, err := receiver.Factory.PrepareBinding(ctx, receiver.Lanes, binding, queueWait)
	if err != nil {
		return PreparedStarter{}, err
	}
	return PreparedStarter{Close: prepared.Close, Start: func(ctx context.Context, binding v2signal.Binding) (Attempt, error) {
		return receiver.Factory.StartPreparedBinding(ctx, receiver.Signaling, receiver.Lanes, binding, prepared)
	}}, nil
}
