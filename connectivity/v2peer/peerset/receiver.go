package peerset

import (
	"context"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type ReceiverConfig struct {
	Purpose       v2signal.ConnectionPurpose
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

type pathLaneSession struct {
	v2peer.ReceiverLaneSession
	sessionID protocolsession.ProtocolSessionID
	content   ContentLaneOwner
}

func (s pathLaneSession) ProtocolSessionID() protocolsession.ProtocolSessionID { return s.sessionID }

func (s pathLaneSession) DetachAdmittedLane(lane session.LaneIdentity) bool {
	return s.content.DetachLane(lane)
}

func pathLanes(lanes v2peer.ReceiverLaneSession, sessionID protocolsession.ProtocolSessionID) v2peer.ReceiverLaneSession {
	content, ok := lanes.(ContentLaneOwner)
	if !ok {
		return lanes
	}
	return pathLaneSession{ReceiverLaneSession: lanes, sessionID: sessionID, content: content}
}

func (owner *PeerSet) openDirect(ctx context.Context, receiver ReceiverConfig) (*Path, error) {
	var sessionID protocolsession.ProtocolSessionID
	if session, ok := receiver.Lanes.(interface {
		ProtocolSessionID() protocolsession.ProtocolSessionID
	}); ok {
		sessionID = session.ProtocolSessionID()
	}
	var controls PeerPathControlSession
	if session, ok := receiver.Lanes.(PeerPathControlSession); ok {
		controls = session
	}
	attemptLanes := pathLanes(receiver.Lanes, sessionID)
	pathConfig := PathConfig{Purpose: receiver.Purpose, SessionID: sessionID, Demand: receiver.Demand, StopAfterWave: receiver.StopAfterWave,
		Native: receiver.Factory.NativeConnectivity(), Controls: controls,
		Start: func(ctx context.Context, binding v2signal.Binding) (Attempt, error) {
			return receiver.Factory.StartBinding(ctx, receiver.Signaling, attemptLanes, binding)
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
		pathConfig.Prepare = func(ctx context.Context, binding v2signal.Binding) (PreparedStarter, error) {
			prepared, err := receiver.Factory.PrepareBinding(ctx, attemptLanes, binding)
			if err != nil {
				return PreparedStarter{}, err
			}
			return PreparedStarter{Close: prepared.Close, Start: func(ctx context.Context, binding v2signal.Binding) (Attempt, error) {
				return receiver.Factory.StartPreparedBinding(ctx, receiver.Signaling, attemptLanes, binding, prepared)
			}}, nil
		}
	}
	return owner.Open(ctx, pathConfig)
}
