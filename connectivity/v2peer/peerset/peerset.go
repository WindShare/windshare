package peerset

import (
	"context"
	"crypto/rand"
	"io"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/peerslot"
	"github.com/windshare/windshare/core/session/protocolsession"

	sessioncontract "github.com/windshare/windshare/core/session"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

type Timer interface {
	C() <-chan time.Time
	Stop()
}
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}
type systemClock struct{}
type systemTimer struct{ *time.Timer }

func (systemClock) Now() time.Time                 { return time.Now() }
func (systemClock) NewTimer(d time.Duration) Timer { return systemTimer{time.NewTimer(d)} }
func (t systemTimer) C() <-chan time.Time          { return t.Timer.C }
func (t systemTimer) Stop()                        { t.Timer.Stop() }

type Attempt interface {
	Ready() <-chan struct{}
	Done() <-chan struct{}
	Lane() (sessioncontract.LaneIdentity, bool)
	Outcome() v2peer.ReceiverAttemptOutcome
	Err() error
	Close() error
}
type Starter func(context.Context, v2signal.Binding) (Attempt, error)
type PreparedStarter struct {
	Start Starter
	Close func()
}
type Preparer func(context.Context, v2signal.Binding) (PreparedStarter, error)
type Demand uint8

const (
	NoDemand Demand = iota
	BrowseDemand
	ContentDemand
)

type Result struct {
	Scope   protocolsession.PeerFailureRecoveryScope
	Cause   error
	Stopped bool
}
type Config struct {
	Budget   *Budget
	Capacity *Capacity
	Clock    Clock
	Random   io.Reader
}

type PeerPathControlSession interface {
	SetPeerPathControlHandler(func(context.Context, []byte) error)
	SendPeerPathControl(context.Context, []byte) error
}

// ContentLaneOwner keeps adopted data transports under session ownership;
// ending negotiation alone cannot retire an admitted lane.
type ContentLaneOwner interface {
	LaneSet() *transferlanes.Set
	DetachLane(sessioncontract.LaneIdentity) bool
}

type PathConfig struct {
	Purpose       v2signal.ConnectionPurpose
	Budget        *Budget
	ICE           iceconfig.Connection
	Native        *nativepeer.NativePeerConnectivity
	Controls      PeerPathControlSession
	SessionID     protocolsession.ProtocolSessionID
	PeerPathID    v2signal.PeerPathID
	Start         Starter
	Prepare       Preparer
	Demand        Demand
	StopAfterWave bool
	Content       ContentLaneOwner
}

func (p *Path) budget() *Budget {
	if p.config.Budget != nil {
		return p.config.Budget
	}
	return p.owner.config.Budget
}

type pathKey struct {
	session protocolsession.ProtocolSessionID
	path    v2signal.PeerPathID
}
type PeerSet struct {
	sequence  uint64
	mu        sync.Mutex
	config    Config
	paths     map[pathKey]*Path
	prewarmed map[protocolsession.ProtocolSessionID]bool
	slots     map[protocolsession.ProtocolSessionID]*peerslot.Pool
}

func New(config Config) (*PeerSet, error) {
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	if config.Budget == nil {
		config.Budget = NewBudget(config.Clock.Now())
	}
	if config.Capacity == nil {
		return nil, ErrConfig
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	return &PeerSet{config: config, paths: make(map[pathKey]*Path), prewarmed: make(map[protocolsession.ProtocolSessionID]bool), slots: make(map[protocolsession.ProtocolSessionID]*peerslot.Pool)}, nil
}
func (s *PeerSet) Open(ctx context.Context, config PathConfig) (*Path, error) {
	if ctx == nil || (config.Start == nil && config.Prepare == nil) || config.Demand > ContentDemand || config.Purpose > v2signal.ThroughputExploration {
		return nil, ErrConfig
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if config.PeerPathID == (v2signal.PeerPathID{}) {
		if _, err := io.ReadFull(s.config.Random, config.PeerPathID[:]); err != nil {
			return nil, err
		}
	}
	key := pathKey{config.SessionID, config.PeerPathID}
	if _, exists := s.paths[key]; exists || len(s.paths) >= MaximumPaths {
		return nil, ErrConfig
	}
	if config.Demand == BrowseDemand && s.prewarmed[config.SessionID] {
		return nil, ErrPathTerminal
	}
	pool := s.slots[config.SessionID]
	if pool == nil {
		pool = &peerslot.Pool{}
		s.slots[config.SessionID] = pool
	}
	slot, err := pool.Acquire()
	if err != nil {
		return nil, err
	}
	if config.Demand == BrowseDemand {
		if config.Native != nil && !config.Native.ClaimPrewarm([16]byte(config.SessionID)) {
			slot.Close()
			return nil, ErrPathTerminal
		}
		s.prewarmed[config.SessionID] = true
	}
	child, cancel := context.WithCancel(ctx)
	if config.Purpose == v2signal.ThroughputExploration {
		config.Budget = NewBudget(s.config.Clock.Now())
		config.StopAfterWave = true
	}
	var supply *transferlanes.RouteSupply
	if config.Content != nil {
		route := transferlanes.RouteDirect
		if _, relayed := config.ICE.Relay(); relayed {
			route = transferlanes.RouteTURN
		}
		supply, err = config.Content.LaneSet().ExpectRoute(route)
		if err != nil {
			cancel()
			slot.Close()
			return nil, err
		}
	}
	s.sequence++
	path := &Path{sequence: s.sequence, owner: s, config: config, key: key, ctx: child, cancel: cancel, slot: slot, supply: supply,
		resourceChanges: make(chan struct{}, 1), demand: config.Demand, wake: make(chan struct{}, 1), done: make(chan struct{}), ready: make(chan struct{})}
	s.paths[key] = path
	go path.run()
	return path, nil
}

type Path struct {
	sequence          uint64
	networkGeneration uint64
	restartCause      error
	mappingPending    bool
	supply            *transferlanes.RouteSupply
	resourceActive    bool
	retired           bool
	resourceChanges   chan struct{}
	deferred          *reservedAttempt
	owner             *PeerSet
	config            PathConfig
	key               pathKey
	ctx               context.Context
	cancel            context.CancelFunc
	mu                sync.Mutex
	demand            Demand
	slot              *peerslot.Lease
	current           Attempt
	binding           v2signal.Binding
	lane              sessioncontract.LaneIdentity
	result            Result
	wake              chan struct{}
	done, ready       chan struct{}
	readyOnce         sync.Once
}

func (p *Path) SetDemand(demand Demand) error {
	if demand > ContentDemand {
		return ErrConfig
	}
	p.mu.Lock()
	p.demand = demand
	p.mu.Unlock()
	p.resourceDemand(false)
	if demand == NoDemand {
		p.cancel()
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return nil
}
func (p *Path) currentDemand() Demand  { p.mu.Lock(); defer p.mu.Unlock(); return p.demand }
func (p *Path) Ready() <-chan struct{} { return p.ready }
func (p *Path) Done() <-chan struct{}  { return p.done }
func (p *Path) Lane() (sessioncontract.LaneIdentity, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil || p.lane.ID == 0 {
		return sessioncontract.LaneIdentity{}, false
	}
	select {
	case <-p.current.Done():
		return sessioncontract.LaneIdentity{}, false
	default:
		return p.lane, true
	}
}
func (p *Path) Result() Result       { p.mu.Lock(); defer p.mu.Unlock(); return p.result }
func (p *Path) Err() error           { return p.Result().Cause }
func (p *Path) Close() error         { p.cancel(); <-p.done; return p.Err() }
func (p *Path) finish(result Result) { p.mu.Lock(); p.result = result; p.mu.Unlock() }
func (p *Path) wait(delay time.Duration) bool {
	timer := p.owner.config.Clock.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return false
		case <-p.wake:
			if p.mappingReady() || p.consumeRestart() != nil {
				return true
			}
			if p.currentDemand() == NoDemand {
				return false
			}
		case <-timer.C():
			return true
		}
	}
}
