package nativepeer

import (
	"context"
	"encoding/binary"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/pionpeer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const MaintenanceInterval = time.Second
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
	path.remoteProfile = pionpeer.TCPProfile(control.ProviderProfile)
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
		if changed && path.content && path.lease != nil {
			n.wakeMaintenanceLocked()
		}
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
		Kind: kind, ProviderProfile: string(pionpeer.LocalTCPProfile()),
	}
	if kind != protocolsession.PeerPathRevoke {
		control.ValidFor = ControlLifetime
	}
	if kind == protocolsession.PeerPathDemand && content {
		control.HoldFor = ControlLifetime
	}
	return protocolsession.EncodePeerPathControl(control)
}

// RetainMaintenance keeps the owner's single background maintenance loop alive
// while at least one session or receiver path is active. Releasing the last
// claim stops and joins the loop before returning.
func (n *NativePeerConnectivity) RetainMaintenance() func() {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return func() {}
	}
	n.maintainers++
	if n.maintainers == 1 {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		prev := n.maintDraining
		n.maintCancel = cancel
		n.maintDone = done
		n.maintDraining = nil
		n.maintWG.Go(func() { n.runMaintenance(ctx, prev, done) })
	}
	n.mu.Unlock()
	return sync.OnceFunc(func() {
		n.mu.Lock()
		n.maintainers--
		var cancel context.CancelFunc
		var done chan struct{}
		if n.maintainers == 0 && n.maintCancel != nil {
			cancel, done = n.maintCancel, n.maintDone
			n.maintCancel = nil
			n.maintDone = nil
			n.maintDraining = done
		}
		n.mu.Unlock()
		if cancel != nil {
			cancel()
			<-done
		}
	})
}

func (n *NativePeerConnectivity) runMaintenance(ctx context.Context, prev <-chan struct{}, done chan struct{}) {
	defer close(done)
	if prev != nil {
		select {
		case <-ctx.Done():
			return
		case <-prev:
		}
	}
	var networkChanges <-chan networkstate.Snapshot
	if subscriber, ok := n.config.Monitor.(networkSubscriber); ok {
		var unsubscribe func()
		networkChanges, unsubscribe = subscriber.Subscribe()
		defer unsubscribe()
		// Sync the current generation only after subscribing so transitions before
		// or during subscription cannot be missed; applySnapshot deduplicates
		// monotonically if the same generation also arrives on networkChanges.
		if n.applySnapshot(subscriber.Current()) && n.hasActiveMaintenance() {
			n.Maintain(ctx)
		}
	}
	ticker := time.NewTicker(MaintenanceInterval)
	defer ticker.Stop()
	discoveryChanges := n.config.Discovery.Changes()
	for {
		select {
		case <-ctx.Done():
			return
		case snapshot, ok := <-networkChanges:
			if !ok {
				networkChanges = nil
				continue
			}
			if n.applySnapshot(snapshot) && n.hasActiveMaintenance() {
				n.Maintain(ctx)
			}
		case <-discoveryChanges:
			n.mu.Lock()
			authority := n.config.Reachability
			n.mu.Unlock()
			if authority != nil {
				authority.RefreshUnavailable()
			}
			if n.hasActiveMaintenance() {
				n.Maintain(ctx)
			}
		case <-n.maintWake:
			if n.hasActiveMaintenance() {
				n.Maintain(ctx)
			}
		case <-ticker.C:
			if n.hasActiveMaintenance() {
				n.Maintain(ctx)
			}
		}
	}
}

func (n *NativePeerConnectivity) hasActiveMaintenance() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, path := range n.paths {
		if path.lease != nil || path.content {
			return true
		}
	}
	return false
}

func (n *NativePeerConnectivity) wakeMaintenanceLocked() {
	select {
	case n.maintWake <- struct{}{}:
	default:
	}
}

func (n *NativePeerConnectivity) notifyLocked(session [16]byte, path *pathResources, change Change) {
	notify(path, change)
	if change.Remote {
		return
	}
	for _, ch := range n.sessionWaiters[session] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (n *NativePeerConnectivity) watchSession(session [16]byte) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	n.nextWaiterID++
	id := n.nextWaiterID
	if n.sessionWaiters[session] == nil {
		n.sessionWaiters[session] = make(map[uint64]chan struct{})
	}
	n.sessionWaiters[session][id] = ch
	n.mu.Unlock()
	return ch, sync.OnceFunc(func() {
		n.mu.Lock()
		if waiters := n.sessionWaiters[session]; waiters != nil {
			delete(waiters, id)
			if len(waiters) == 0 {
				delete(n.sessionWaiters, session)
			}
		}
		n.mu.Unlock()
	})
}

// RunSession maintains demand facts and publishes local path notices for one
// session without allocating a per-session maintenance ticker.
func (n *NativePeerConnectivity) RunSession(ctx context.Context, session [16]byte, send func(context.Context, []byte) error) {
	if ctx.Err() != nil {
		return
	}
	releaseMaintenance := n.RetainMaintenance()
	defer releaseMaintenance()
	wake, releaseSession := n.watchSession(session)
	defer releaseSession()
	n.flushSessionControls(ctx, session, send)
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			n.flushSessionControls(ctx, session, send)
		}
	}
}

func (n *NativePeerConnectivity) flushSessionControls(ctx context.Context, session [16]byte, send func(context.Context, []byte) error) {
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
