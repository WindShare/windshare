// Package nativepeer assembles demand-owned native connectivity resources.
// It reports changed facts; the PeerSet alone decides whether to start an attempt.
package nativepeer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/pionpeer"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/reachability/gateway"
	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/observationstream"
)

const (
	MaximumPaths = 64
	WaveLifetime = 120 * time.Second
)

type AttemptRequest struct {
	Configuration     pion.Configuration
	ProtocolSessionID [16]byte
	Binding           v2signal.Binding
}

// NetworkMonitor provides concurrency-safe network state polling.
type NetworkMonitor interface {
	Poll(context.Context, time.Time) (networkstate.Snapshot, bool, error)
}

type networkSubscriber interface {
	Subscribe() (<-chan networkstate.Snapshot, func())
	Current() networkstate.Snapshot
}
type Config struct {
	Admission           *Admission
	Connect             func(pion.Configuration, pionpeer.AttemptConfig) (*pionpeer.Connection, error)
	Monitor             NetworkMonitor
	Sockets             *socketauthority.Authority
	Pool                func() icepolicy.ICEEndpointPool
	Reachability        *reachability.Authority
	Discovery           *gateway.Discovery
	Now                 func() time.Time
	ObservationCapacity int
	Side                string
}
type Change struct {
	RemoteNetworkChanged bool
	Remote               bool
	Retired              bool
	NetworkGenerationID  uint64
	MappingReady         bool
}
type pathKey struct {
	session [16]byte
	path    v2signal.PeerPathID
}
type pathResources struct {
	optionalDone                  chan struct{}
	releaseOptional               func()
	localSequence, remoteSequence uint64
	remoteGeneration              [16]byte
	remoteProfile                 pionpeer.TCPProfile
	remoteDemand                  bool
	retired                       bool
	remoteProfileExpires          time.Time
	remoteExpires, holdUntil      time.Time
	lease                         *socketauthority.Lease
	generation                    uint64
	content, direct               bool
	mapped                        bool
	mappedLocal                   netip.AddrPort
	mappedProtocol                reachability.Protocol
	changes                       chan Change
	attempts                      map[*PreparedAttempt]struct{}
	waveStarted                   time.Time
	profileStage                  int
	primaryProfile, backupProfile icepolicy.AttemptICEProfile
	endpointIDs, failureDomains   []string
}
type NativePeerConnectivity struct {
	mu                    sync.Mutex
	cleanup               sync.WaitGroup
	operations            sync.WaitGroup
	maintWG               sync.WaitGroup
	maintainers           int
	maintCancel           context.CancelFunc
	maintDone             chan struct{}
	maintDraining         chan struct{}
	maintWake             chan struct{}
	sessionWaiters        map[[16]byte]map[uint64]chan struct{}
	nextWaiterID          uint64
	producer              observationstream.Producer[Observation]
	observations          observationstream.Consumer[Observation]
	closed                bool
	prewarmed             map[[16]byte]bool
	config                Config
	paths                 map[pathKey]*pathResources
	facts                 *icepolicy.FactStore
	generation            uint64
	automaticReachability bool
	snapshotState         networkstate.Snapshot
}

var ErrCapacityRequired = errors.New("native connectivity requires admission")

func New(config Config) (*NativePeerConnectivity, error) {
	if config.Admission == nil {
		return nil, ErrCapacityRequired
	}
	if config.Monitor == nil {
		config.Monitor = networkstate.NewMonitor(nil, networkstate.DefaultDebounce)
	}
	if config.Discovery == nil {
		config.Discovery = gateway.NewDiscovery(nil)
	}
	if config.Connect == nil {
		config.Connect = pionpeer.NewPeerConnection
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	owner := &NativePeerConnectivity{
		config:                config,
		paths:                 make(map[pathKey]*pathResources),
		prewarmed:             make(map[[16]byte]bool),
		maintWake:             make(chan struct{}, 1),
		sessionWaiters:        make(map[[16]byte]map[uint64]chan struct{}),
		automaticReachability: config.Reachability == nil,
	}
	if config.ObservationCapacity > 0 {
		capacity := min(config.ObservationCapacity, DefaultObservationCapacity)
		owner.producer, owner.observations, _ = observationstream.New[Observation](observationstream.Capacity(capacity))
	}
	if owner.config.Sockets == nil {
		sockets := socketauthority.Config{}
		if config.ObservationCapacity > 0 {
			sockets.Observe = owner.observeSocket
		}
		owner.config.Sockets = socketauthority.New(sockets)
	}
	return owner, nil
}
func (n *NativePeerConnectivity) pathLocked(key pathKey) (*pathResources, error) {
	if n.closed {
		return nil, errors.New("native peer connectivity closed")
	}
	if key.path == (v2signal.PeerPathID{}) {
		return nil, errors.New("native peer path identity is zero")
	}
	if path := n.paths[key]; path != nil {
		if path.retired {
			return nil, errors.New("native peer path retired")
		}
		return path, nil
	}
	if len(n.paths) >= MaximumPaths {
		return nil, errors.New("native peer path capacity exhausted")
	}
	path := &pathResources{changes: make(chan Change, 1)}
	n.paths[key] = path
	return path, nil
}

// Demand is the only authority for gateway work. Browsing can allocate an ICE
// path but cannot turn a speculative connection into a router mapping request.
func (n *NativePeerConnectivity) SetDemand(session [16]byte, path v2signal.PeerPathID, content, direct bool) (<-chan Change, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	resources, err := n.pathLocked(pathKey{session, path})
	if err != nil {
		return nil, err
	}
	changed := resources.content != content || resources.direct != direct
	resources.content, resources.direct = content, direct
	if direct {
		for attempt := range resources.attempts {
			attempt.release()
		}
	}
	if changed {
		n.observeLifecycleLocked(pathKey{session, path}, resources, DemandChanged, 0)
	}
	n.refreshDemandLocked(pathKey{session, path}, resources)
	if changed && resources.content && resources.lease != nil {
		n.wakeMaintenanceLocked()
	}
	return resources.changes, nil
}
func (n *NativePeerConnectivity) Generation(session [16]byte, path v2signal.PeerPathID) uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if resource := n.paths[pathKey{session, path}]; resource != nil {
		return resource.generation
	}
	return 0
}

// NetworkGeneration includes snapshots without a live path, so exhausted
// recovery budgets can be reconsidered after the network changes.
func (n *NativePeerConnectivity) NetworkGeneration() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.generation
}
func (n *NativePeerConnectivity) NewPeerConnection(ctx context.Context, request AttemptRequest) (*pionpeer.Connection, error) {
	prepared, err := n.PrepareAttempt(ctx, request)
	if err != nil {
		return nil, err
	}
	defer prepared.Close()
	return prepared.Start(ctx)
}
func (n *NativePeerConnectivity) snapshot(ctx context.Context) (networkstate.Snapshot, error) {
	snapshot, _, err := n.config.Monitor.Poll(ctx, n.config.Now())
	if err != nil {
		return snapshot, err
	}
	n.applySnapshot(snapshot)
	return snapshot, nil
}

// applySnapshot advances local network generation monotonically. It returns
// true only when an established generation transitions to a newer one so
// maintenance callers reconcile active paths without duplicating baseline setup.
func (n *NativePeerConnectivity) applySnapshot(snapshot networkstate.Snapshot) bool {
	if snapshot.GenerationID() == 0 {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || snapshot.GenerationID() <= n.generation {
		return false
	}

	previous := n.generation
	n.generation = snapshot.GenerationID()
	n.facts = icepolicy.NewFactStore(fmt.Sprint(n.generation))
	n.snapshotState = snapshot
	if n.automaticReachability {
		previousAuthority := n.config.Reachability
		n.config.Reachability = reachability.New(reachability.Config{Now: n.config.Now, Gateways: n.config.Discovery.Gateways(snapshot), Observe: n.observeReachability})
		if previousAuthority != nil {
			previousAuthority.Retire(previous)
			n.scheduleCleanupLocked(previousAuthority, true)
		}
	}
	if previous != 0 {
		n.config.Sockets.Retire(previous)
		if n.config.Reachability != nil {
			n.config.Reachability.Retire(previous)
		}
		for key, path := range n.paths {
			for attempt := range path.attempts {
				if attempt.snapshot.GenerationID() != n.generation {
					attempt.cancel()
				}
			}
			n.notifyLocked(key.session, path, Change{NetworkGenerationID: n.generation})
		}
		return true
	}
	return false
}

// Maintain is called by the demand owner. It never creates a PeerConnection or
// renews the application's demand lifetime.
func (n *NativePeerConnectivity) Maintain(ctx context.Context) {
	_, _ = n.snapshot(ctx)
	n.refreshDemands()
	n.mu.Lock()
	authority := n.config.Reachability
	n.mu.Unlock()
	if authority != nil {
		select {
		case <-n.config.Discovery.Changes():
			authority.RefreshUnavailable()
		default:
		}
	}
	n.reconcileReachability(ctx)
}

// refreshDemands expires stale remote demand windows and synchronizes active
// path demand into the current reachability authority without blocking on I/O.
func (n *NativePeerConnectivity) refreshDemands() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.config.Now()
	active := false
	for key, path := range n.paths {
		if path.content && path.remoteDemand && (!now.Before(path.remoteExpires) || !now.Before(path.holdUntil)) {
			path.content = false
			n.observeLifecycleLocked(key, path, DemandChanged, 0)
		}
		n.refreshDemandLocked(key, path)
		if path.lease != nil || path.content {
			active = true
		}
	}
	return active
}

func (n *NativePeerConnectivity) refreshUnavailable() {
	n.mu.Lock()
	authority := n.config.Reachability
	n.mu.Unlock()
	if authority != nil {
		authority.RefreshUnavailable()
	}
}

func (n *NativePeerConnectivity) reconcileReachability(ctx context.Context) {
	n.mu.Lock()
	authority := n.config.Reachability
	n.mu.Unlock()
	if authority != nil {
		authority.Reconcile(ctx)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	for key, path := range n.paths {
		mapped := n.hasMappingLocked(path)
		if mapped && !path.mapped {
			n.notifyLocked(key.session, path, Change{NetworkGenerationID: path.generation, MappingReady: true})
		}
		path.mapped = mapped
	}
}
func notify(path *pathResources, change Change) {
	select {
	case path.changes <- change:
	default:
		select {
		case pending := <-path.changes:
			if pending.Retired {
				change = pending
			}
		default:
		}
		select {
		case path.changes <- change:
		default:
		}
	}
}
func demandID(key pathKey, index int) string {
	return hex.EncodeToString(key.session[:]) + ":" + hex.EncodeToString(key.path[:]) + fmt.Sprint(":", index)
}
func (n *NativePeerConnectivity) ClosePath(session [16]byte, path v2signal.PeerPathID) {
	n.mu.Lock()
	key := pathKey{session, path}
	resource := n.paths[key]
	if resource == nil {
		n.mu.Unlock()
		return
	}
	delete(n.paths, key)
	if resource.optionalDone != nil {
		close(resource.optionalDone)
	}
	var attempts []*PreparedAttempt
	for attempt := range resource.attempts {
		attempt.cancel()
		attempts = append(attempts, attempt)
	}
	n.observeLifecycleLocked(key, resource, PathClosed, 0)
	n.releaseLeaseLocked(key, resource)
	n.mu.Unlock()
	for _, attempt := range attempts {
		attempt.abort()
	}
	if resource.releaseOptional != nil {
		resource.releaseOptional()
	}
}
func (n *NativePeerConnectivity) CloseSession(session [16]byte) {
	n.mu.Lock()
	delete(n.prewarmed, session)
	var paths []v2signal.PeerPathID
	for key := range n.paths {
		if key.session == session {
			paths = append(paths, key.path)
		}
	}
	n.mu.Unlock()
	for _, path := range paths {
		n.ClosePath(session, path)
	}
}

func (n *NativePeerConnectivity) egressLocked(address netip.Addr) string {
	for _, entry := range n.snapshotState.Addresses() {
		if entry.IP == address {
			return gateway.EgressID(entry.InterfaceIndex)
		}
	}
	return ""
}

func (n *NativePeerConnectivity) releaseLeaseLocked(key pathKey, path *pathResources) {
	if path.mapped && !n.closed {
		n.scheduleCleanupLocked(n.config.Reachability, false)
	}
	if path.lease != nil {
		if n.config.Reachability != nil {
			for index := range n.pathEndpointsLocked(path) {
				n.config.Reachability.Withdraw(demandID(key, index))
			}
		}
		_ = path.lease.Close()
	}
	path.lease = nil
}
