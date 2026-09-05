package nativepeer

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

const MaintenanceInterval = 250 * time.Millisecond
const ControlLifetime = 60 * time.Second
const ControlRefresh = 30 * time.Second

func GenerationIdentity(generation uint64) (identity [16]byte) {
	binary.BigEndian.PutUint64(identity[8:], generation)
	return identity
}

// ApplyControl sees only the core runtime's authenticated canonical body. A
// watermark survives expiry/revoke so late delivery cannot resurrect demand.
func (n *NativePeerConnectivity) ApplyControl(session [16]byte, body []byte) (protocolsession.PeerPathControl, bool) {
	control, err := protocolsession.DecodePeerPathControl(body)
	if err != nil {
		return control, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	key := pathKey{session, v2signal.PeerPathID(control.PeerPathID)}
	if n.paths[key] == nil && control.Kind != protocolsession.PeerPathDemand {
		return control, false
	}
	path, err := n.pathLocked(key)
	if err != nil || control.ControlSequence <= path.remoteSequence {
		return control, false
	}
	if path.remoteGeneration != ([16]byte{}) && path.remoteGeneration != control.NetworkGenerationID && control.Kind != protocolsession.PeerPathNetworkChanged && control.Kind != protocolsession.PeerPathDemand {
		return control, false
	}
	path.remoteSequence = control.ControlSequence
	path.remoteGeneration = control.NetworkGenerationID
	path.remoteProfile = provider.TCPProfile(control.ProviderProfile)
	path.remoteProfileExpires = n.config.Now().Add(control.ValidFor)
	switch control.Kind {
	case protocolsession.PeerPathDemand:
		changed := path.content != (control.HoldFor > 0)
		path.remoteDemand = true
		path.remoteExpires = n.config.Now().Add(control.ValidFor)
		path.content = control.HoldFor > 0
		path.holdUntil = n.config.Now().Add(control.HoldFor)
		if changed {
			n.observeLifecycleLocked(key, path, DemandChanged, 0)
		}
		n.refreshDemandLocked(key, path)
	case protocolsession.PeerPathRevoke:
		path.remoteDemand = true
		path.remoteExpires = n.config.Now()
		path.content = false
		path.retired = true
		for attempt := range path.attempts {
			attempt.cancel()
		}
		n.observeLifecycleLocked(key, path, PathClosed, 0)
		n.releaseLeaseLocked(key, path)
		notify(path, Change{Retired: true, Remote: true, NetworkGenerationID: path.generation})
	case protocolsession.PeerPathMappingReady:
		notify(path, Change{NetworkGenerationID: path.generation, MappingReady: true, Remote: true})
	case protocolsession.PeerPathNetworkChanged:
		notify(path, Change{NetworkGenerationID: path.generation, RemoteNetworkChanged: true, Remote: true})
	}
	return control, true
}
func (n *NativePeerConnectivity) Control(session [16]byte, pathID v2signal.PeerPathID, kind protocolsession.PeerPathControlKind, content bool) ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	path, err := n.pathLocked(pathKey{session, pathID})
	if err != nil {
		return nil, err
	}
	path.localSequence++
	generation := path.generation
	if generation == 0 {
		generation = n.generation
	}
	if generation == 0 {
		generation = 1
	}
	control := protocolsession.PeerPathControl{
		PeerPathID: [16]byte(pathID), NetworkGenerationID: GenerationIdentity(generation), ControlSequence: path.localSequence,
		Kind: kind, ProviderProfile: string(provider.LocalTCPProfile()),
	}
	if kind != protocolsession.PeerPathRevoke {
		control.ValidFor = ControlLifetime
	}
	if kind == protocolsession.PeerPathDemand && content {
		control.HoldFor = ControlLifetime
	}
	return protocolsession.EncodePeerPathControl(control)
}

// RunSession maintains demand facts only; it cannot allocate attempts.
func (n *NativePeerConnectivity) RunSession(ctx context.Context, session [16]byte, send func(context.Context, []byte) error) {
	timer := time.NewTicker(MaintenanceInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			n.maintainSession(ctx, session, send)
		}
	}
}

func (n *NativePeerConnectivity) maintainSession(ctx context.Context, session [16]byte, send func(context.Context, []byte) error) {

	n.mu.Lock()
	active := false
	for key, path := range n.paths {
		if key.session == session && (path.lease != nil || path.content) {
			active = true
			break
		}
	}
	n.mu.Unlock()
	if !active {
		return
	}
	n.Maintain(ctx)
	if send == nil {
		return
	}
	n.mu.Lock()
	var pending []struct {
		path v2signal.PeerPathID
		kind protocolsession.PeerPathControlKind
	}
	for key, path := range n.paths {
		if key.session != session {
			continue
		}
		select {
		case change := <-path.changes:
			if change.Remote {
				continue
			}
			kind := protocolsession.PeerPathNetworkChanged
			if change.MappingReady {
				kind = protocolsession.PeerPathMappingReady
			}
			pending = append(pending, struct {
				path v2signal.PeerPathID
				kind protocolsession.PeerPathControlKind
			}{key.path, kind})
		default:
		}
	}
	n.mu.Unlock()
	for _, change := range pending {
		body, err := n.Control(session, change.path, change.kind, false)
		if err == nil {
			_ = send(ctx, body)
		}
	}
}
