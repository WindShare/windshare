package revisioncapacity

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

var processOwnerSequence atomic.Uint64

type ProcessOwner struct {
	coordinator *Coordinator
}

func NewProcessOwner(config ProcessConfig) (*ProcessOwner, error) {
	if err := validateLimits(config.Limits); err != nil {
		return nil, err
	}
	if err := validateRetryAfter(config.RetryAfter); err != nil {
		return nil, err
	}
	ownerID := processOwnerSequence.Add(1)
	coordinator := &Coordinator{
		ownerID:    ownerID,
		limits:     config.Limits,
		retryAfter: config.RetryAfter,
		tracer:     config.Tracer,
		stores:     make(map[StoreID]*storeState),
		shares:     make(map[ShareID]*storeState),
		candidates: make(map[candidateKey]*candidateState),
		byCharge:   make(map[*chargeState]*candidateState),
	}
	coordinator.cond = sync.NewCond(&coordinator.mu)
	return &ProcessOwner{coordinator: coordinator}, nil
}

func (o *ProcessOwner) Coordinator() *Coordinator {
	if o == nil {
		return nil
	}
	return o.coordinator
}

func (o *ProcessOwner) Close() error {
	if o == nil || o.coordinator == nil {
		return nil
	}
	c := o.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if len(c.stores) != 0 {
		return &LiveStoreRegistrationsError{count: len(c.stores)}
	}
	c.closed = true
	return nil
}

type Coordinator struct {
	mu   sync.Mutex
	cond *sync.Cond

	ownerID      uint64
	nextDecision uint64
	nextClaim    uint64
	closed       bool
	limits       CapacityLimits
	used         CapacityUsage
	pending      uint64
	reclaimable  uint64
	quarantined  uint64
	retryAfter   time.Duration
	tracer       Tracer

	stores         map[StoreID]*storeState
	shares         map[ShareID]*storeState
	candidates     map[candidateKey]*candidateState
	byCharge       map[*chargeState]*candidateState
	activeReclaims uint64
}

type storeState struct {
	target      ReclaimTarget
	storeID     StoreID
	shareID     ShareID
	limits      CapacityLimits
	used        CapacityUsage
	reclaimable uint64
	quarantined uint64
	closing     bool
	closed      bool
	pending     uint64
	reclaims    uint64
	liveCharges uint64
	sessions    map[SessionID]*sessionState
}

type sessionState struct {
	store       *storeState
	sessionID   SessionID
	limits      CapacityLimits
	used        CapacityUsage
	closing     bool
	closed      bool
	pending     uint64
	liveCharges uint64
}

func (c *Coordinator) RegisterStore(config StoreConfig, target ReclaimTarget) (*StoreRegistration, error) {
	if c == nil {
		return nil, errors.New("revision capacity store registration requires a coordinator")
	}
	if config.StoreID == "" || config.ShareID == "" || target == nil {
		return nil, errors.New("revision capacity store registration requires store/share identities and a reclaim target")
	}
	if err := validateLimits(config.Limits); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrCoordinatorClosed
	}
	if _, exists := c.stores[config.StoreID]; exists {
		return nil, fmt.Errorf("revision capacity store identity %q is already registered", config.StoreID)
	}
	// One capacity authority per share prevents multiple stores from silently
	// multiplying the configured share budget.
	if _, exists := c.shares[config.ShareID]; exists {
		return nil, fmt.Errorf("revision capacity share identity %q is already registered", config.ShareID)
	}
	state := &storeState{
		target: target, storeID: config.StoreID, shareID: config.ShareID,
		limits: config.Limits, sessions: make(map[SessionID]*sessionState),
	}
	registration := &StoreRegistration{coordinator: c, state: state}
	c.stores[state.storeID] = state
	c.shares[state.shareID] = state
	return registration, nil
}

func (c *Coordinator) nextDecisionIDLocked() CapacityDecisionID {
	c.nextDecision++
	return CapacityDecisionID(fmt.Sprintf("capacity-%d-%d", c.ownerID, c.nextDecision))
}

func (c *Coordinator) trace(event TraceEvent) {
	if c == nil || c.tracer == nil {
		return
	}
	defer func() { _ = recover() }()
	c.tracer.TraceCapacity(event)
}

func (c *Coordinator) snapshotLocked(store *storeState) CapacitySnapshot {
	snapshot := CapacitySnapshot{
		process: ScopeSnapshot{
			scope: CapacityScopeProcess, identity: fmt.Sprintf("process-%d", c.ownerID), limits: c.limits,
			used: c.used, reclaimable: c.reclaimable, quarantined: c.quarantined,
			pending: c.pending, reclaims: c.activeReclaims,
		},
	}
	if store == nil {
		return snapshot
	}
	snapshot.closed = store.closed
	snapshot.share = ScopeSnapshot{
		scope: CapacityScopeShare, identity: string(store.shareID), limits: store.limits,
		used: store.used, reclaimable: store.reclaimable, quarantined: store.quarantined,
		pending: store.pending, reclaims: store.reclaims,
	}
	sessionIDs := make([]SessionID, 0, len(store.sessions))
	for sessionID := range store.sessions {
		sessionIDs = append(sessionIDs, sessionID)
	}
	slices.Sort(sessionIDs)
	snapshot.sessions = make([]ScopeSnapshot, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		session := store.sessions[sessionID]
		snapshot.sessions = append(snapshot.sessions, ScopeSnapshot{
			scope: CapacityScopeSession, identity: string(session.sessionID), limits: session.limits,
			used: session.used, pending: session.pending,
		})
	}
	return snapshot
}

// decisionSnapshotLocked is intentionally constant-time. Admission and
// settlement traces need the deciding process/share/session facts, not a scan of
// every sibling session registered in the share.
func (c *Coordinator) decisionSnapshotLocked(store *storeState, session *sessionState) CapacitySnapshot {
	snapshot := CapacitySnapshot{
		process: ScopeSnapshot{
			scope: CapacityScopeProcess, identity: fmt.Sprintf("process-%d", c.ownerID), limits: c.limits,
			used: c.used, reclaimable: c.reclaimable, quarantined: c.quarantined,
			pending: c.pending, reclaims: c.activeReclaims,
		},
	}
	if store == nil {
		return snapshot
	}
	snapshot.closed = store.closed
	snapshot.share = ScopeSnapshot{
		scope: CapacityScopeShare, identity: string(store.shareID), limits: store.limits,
		used: store.used, reclaimable: store.reclaimable, quarantined: store.quarantined,
		pending: store.pending, reclaims: store.reclaims,
	}
	if session != nil {
		snapshot.sessions = []ScopeSnapshot{{
			scope: CapacityScopeSession, identity: string(session.sessionID), limits: session.limits,
			used: session.used, pending: session.pending,
		}}
	}
	return snapshot
}
