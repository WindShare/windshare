package content

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/core/capacityledger"
)

var handlePoolSequence atomic.Uint64

type capacityTracePublication struct {
	event CapacityTraceEvent
	owner CapacityTracer
}

// HandlePool owns process-wide stable-handle and active-lease ledgers plus the
// idle grace-period LRU pool. Registered RevisionStores serialize handle/lease
// state transitions under HandlePool.mu while running all StableFile I/O outside
// the lock, eliminating dual-lock reclamation callbacks and 2PC handoffs.
type HandlePool struct {
	mu           sync.Mutex
	cond         *sync.Cond
	ownerID      uint64
	nextDecision uint64
	closed       bool
	retryAfter   time.Duration
	tracer       CapacityTracer

	handles *capacityledger.Ledger
	leases  *capacityledger.Ledger
	stores  map[*RevisionStore]struct{}
	idle    idleList
}

func NewHandlePool(config HandlePoolConfig) (*HandlePool, error) {
	if err := validateCapacityLimits(config.Limits); err != nil {
		return nil, err
	}
	if err := validateCapacityRetryAfter(config.RetryAfter); err != nil {
		return nil, err
	}
	pool := &HandlePool{
		ownerID:    handlePoolSequence.Add(1),
		retryAfter: config.RetryAfter,
		tracer:     config.Tracer,
		handles:    capacityledger.New(config.Limits.StableHandles),
		leases:     capacityledger.New(config.Limits.ActiveLeases),
		stores:     make(map[*RevisionStore]struct{}),
		idle:       idleList{global: true},
	}
	pool.cond = sync.NewCond(&pool.mu)
	return pool, nil
}

func (p *HandlePool) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	if len(p.stores) != 0 {
		return &LiveStoresError{count: len(p.stores)}
	}
	p.closed = true
	return errors.Join(p.handles.Close(), p.leases.Close())
}

func (p *HandlePool) Snapshot() ScopeSnapshot {
	if p == nil {
		return ScopeSnapshot{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.processSnapshotLocked()
}

func (p *HandlePool) registerStore(store *RevisionStore, limits CapacityLimits) error {
	if err := validateCapacityLimits(limits); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrHandlePoolClosed
	}
	store.handleLedger = p.handles.Child(limits.StableHandles)
	store.leaseLedger = p.leases.Child(limits.ActiveLeases)
	store.shareLimits = limits
	store.shareLabel = base64.RawURLEncoding.EncodeToString(store.shareInstance.Bytes())
	p.stores[store] = struct{}{}
	return nil
}

func (p *HandlePool) nextDecisionIDLocked() CapacityDecisionID {
	p.nextDecision++
	return CapacityDecisionID(fmt.Sprintf("capacity-%d-%d", p.ownerID, p.nextDecision))
}

func (p *HandlePool) processSnapshotLocked() ScopeSnapshot {
	h, l := p.handles.Snapshot(), p.leases.Snapshot()
	return ScopeSnapshot{
		scope:       CapacityScopeProcess,
		identity:    fmt.Sprintf("process-%d", p.ownerID),
		limits:      CapacityLimits{StableHandles: h.Limit, ActiveLeases: l.Limit},
		used:        CapacityUsage{StableHandles: h.Used, ActiveLeases: l.Used},
		reclaimable: p.idle.count,
	}
}

func (p *HandlePool) storeSnapshotLocked(store *RevisionStore, includeAllSessions bool, focus *RevisionSession) CapacitySnapshot {
	snapshot := CapacitySnapshot{process: p.processSnapshotLocked()}
	if store == nil {
		return snapshot
	}
	h, l := store.handleLedger.Snapshot(), store.leaseLedger.Snapshot()
	snapshot.closed = store.closed
	snapshot.share = ScopeSnapshot{
		scope:       CapacityScopeShare,
		identity:    store.shareLabel,
		limits:      store.shareLimits,
		used:        CapacityUsage{StableHandles: h.Used, ActiveLeases: l.Used},
		reclaimable: store.idle.count,
	}
	if includeAllSessions {
		ids := make([]SessionID, 0, len(store.sessionByID))
		for id := range store.sessionByID {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		snapshot.sessions = make([]ScopeSnapshot, 0, len(ids))
		for _, id := range ids {
			snapshot.sessions = append(snapshot.sessions, store.sessionByID[id].snapshotLocked())
		}
	} else if focus != nil {
		snapshot.sessions = []ScopeSnapshot{focus.snapshotLocked()}
	}
	return snapshot
}

func (p *HandlePool) emitTrace(pub *capacityTracePublication) {
	if p == nil || pub == nil {
		return
	}
	deliverCapacityTrace(pub.owner, pub.event)
	deliverCapacityTrace(p.tracer, pub.event)
}

func deliverCapacityTrace(tracer CapacityTracer, event CapacityTraceEvent) {
	if tracer == nil {
		return
	}
	defer func() { _ = recover() }()
	tracer.TraceCapacity(event)
}

func (p *HandlePool) makeTraceLocked(
	stage CapacityTraceStage,
	decision CapacityDecisionID,
	store *RevisionStore,
	session *RevisionSession,
	revisionID string,
	diagnostic error,
) *capacityTracePublication {
	if store.capacityTracer == nil && p.tracer == nil {
		return nil
	}
	var sessionID SessionID
	if session != nil {
		sessionID = session.id
	}
	return &capacityTracePublication{
		owner: store.capacityTracer,
		event: CapacityTraceEvent{
			stage:      stage,
			decision:   decision,
			shareID:    store.shareLabel,
			sessionID:  sessionID,
			revisionID: revisionID,
			snapshot:   p.storeSnapshotLocked(store, false, session),
			diagnostic: diagnostic,
		},
	}
}

func (p *HandlePool) leaseBlockScopeLocked(store *RevisionStore, session *RevisionSession) CapacityScope {
	if session.leaseLedger.Snapshot().Used >= session.leaseLedger.Limit() {
		return CapacityScopeSession
	}
	if store.leaseLedger.Snapshot().Used >= store.leaseLedger.Limit() {
		return CapacityScopeShare
	}
	return CapacityScopeProcess
}

func (p *HandlePool) denyLocked(
	store *RevisionStore,
	session *RevisionSession,
	revisionID string,
	resource CapacityResource,
	scope CapacityScope,
) (*CapacityBusyError, *capacityTracePublication) {
	decision := p.nextDecisionIDLocked()
	busy := &CapacityBusyError{
		decisionID: decision,
		resource:   resource,
		scope:      scope,
		retryAfter: p.retryAfter,
		snapshot:   p.storeSnapshotLocked(store, false, session),
	}
	trace := p.makeTraceLocked(CapacityTraceDenied, decision, store, session, revisionID, busy)
	return busy, trace
}

type newRevisionReservation struct {
	session    *RevisionSession
	handleHold capacityledger.Hold
	leaseHold  capacityledger.Hold
	fileHold   capacityledger.Hold
	decisionID CapacityDecisionID
}

func (r *newRevisionReservation) releaseLocked(pool *HandlePool) {
	if r == nil {
		return
	}
	r.leaseHold.Release()
	r.fileHold.Release()
	r.handleHold.Release()
	if r.session != nil {
		r.session.pending--
		r.session = nil
		pool.cond.Broadcast()
	}
}

type evictedRevision struct {
	store      *RevisionStore
	revision   *revisionState
	source     StableFile
	identity   revisionIdentity
	decisionID CapacityDecisionID
	requester  *RevisionStore
	session    *RevisionSession
}

func (e *evictedRevision) close() error {
	if e == nil {
		return nil
	}
	defer e.store.closeWG.Done()
	closeErr := closeStableFile(e.source)
	pool := e.store.pool
	pool.mu.Lock()
	e.revision.closing = false
	e.revision.closed = true
	trace := pool.makeTraceLocked(
		CapacityTraceEvicted, e.decisionID, e.requester, e.session,
		capacityRevisionID(e.identity), closeErr,
	)
	pool.mu.Unlock()
	e.store.retireRevisionCache(e.identity)
	pool.emitTrace(trace)
	return closeErr
}

// reserveNewRevisionLocked atomically reserves a new physical handle, active
// lease, and per-session file slot. When the handle ceiling is reached, the
// oldest idle grace-period handle is detached under the same lock so the caller
// can close it outside the lock before opening the replacement file.
func (s *RevisionStore) reserveNewRevisionLocked(
	session *RevisionSession,
	identity revisionIdentity,
) (newRevisionReservation, *evictedRevision, *capacityTracePublication, error) {
	pool := s.pool
	if pool.closed {
		return newRevisionReservation{}, nil, nil, ErrHandlePoolClosed
	}
	if s.closed {
		return newRevisionReservation{}, nil, nil, ErrRevisionStoreClosed
	}
	if session == nil || session.store != s {
		return newRevisionReservation{}, nil, nil, errors.New("revision session belongs to another store")
	}
	if session.closed {
		return newRevisionReservation{}, nil, nil, ErrSessionClosed
	}
	revisionID := capacityRevisionID(identity)
	leaseHold, ok := session.leaseLedger.Reserve(1)
	if !ok {
		busy, trace := pool.denyLocked(s, session, revisionID, CapacityResourceActiveLease, pool.leaseBlockScopeLocked(s, session))
		return newRevisionReservation{}, nil, trace, busy
	}
	fileHold, ok := session.fileLedger.Reserve(1)
	if !ok {
		leaseHold.Release()
		busy, trace := pool.denyLocked(s, session, revisionID, CapacityResourceStableHandle, CapacityScopeSession)
		return newRevisionReservation{}, nil, trace, busy
	}
	var evicted *evictedRevision
	var decisionID CapacityDecisionID
	handleHold, ok := s.handleLedger.Reserve(1)
	if !ok {
		shareBlocked := s.handleLedger.Snapshot().Used >= s.handleLedger.Limit()
		var victim *revisionState
		if shareBlocked {
			victim = s.idle.head
		} else {
			victim = pool.idle.head
		}
		if victim == nil {
			fileHold.Release()
			leaseHold.Release()
			scope := CapacityScopeProcess
			if shareBlocked {
				scope = CapacityScopeShare
			}
			busy, trace := pool.denyLocked(s, session, revisionID, CapacityResourceStableHandle, scope)
			return newRevisionReservation{}, nil, trace, busy
		}
		decisionID = pool.nextDecisionIDLocked()
		evicted = pool.detachIdleVictimLocked(victim, decisionID, s, session)
		handleHold, ok = s.handleLedger.Reserve(1)
		if !ok {
			fileHold.Release()
			leaseHold.Release()
			busy, trace := pool.denyLocked(s, session, revisionID, CapacityResourceStableHandle, CapacityScopeProcess)
			return newRevisionReservation{}, evicted, trace, busy
		}
	}
	if decisionID == "" {
		decisionID = pool.nextDecisionIDLocked()
	}
	session.pending++
	trace := pool.makeTraceLocked(CapacityTraceAdmitted, decisionID, s, session, revisionID, nil)
	return newRevisionReservation{
		session: session, handleHold: handleHold, leaseHold: leaseHold, fileHold: fileHold, decisionID: decisionID,
	}, evicted, trace, nil
}

func (p *HandlePool) detachIdleVictimLocked(
	victim *revisionState,
	decisionID CapacityDecisionID,
	requester *RevisionStore,
	session *RevisionSession,
) *evictedRevision {
	victimStore := victim.store
	p.unlinkIdleLocked(victim)
	if victimStore.revisions[victim.descriptor.FileID()] == victim {
		delete(victimStore.revisions, victim.descriptor.FileID())
	}
	victim.lifecycle = revisionLifecycleReleased
	victim.closePending = true
	victim.closing = true
	source := victim.source
	victim.source = nil
	victim.handleHold.Release()
	victim.handleHold = capacityledger.Hold{}
	victimStore.closeWG.Add(1)
	return &evictedRevision{
		store: victimStore, revision: victim, source: source, identity: victim.identity(),
		decisionID: decisionID, requester: requester, session: session,
	}
}

func (p *HandlePool) markIdleLocked(revision *revisionState) {
	if revision == nil || revision.poolIdle.linked {
		return
	}
	p.idle.insert(revision)
	revision.store.idle.insert(revision)
}

func (p *HandlePool) unlinkIdleLocked(revision *revisionState) {
	if revision == nil || !revision.poolIdle.linked {
		return
	}
	p.idle.remove(revision)
	revision.store.idle.remove(revision)
}

type idleLink struct {
	prev, next *revisionState
	linked     bool
}

type idleList struct {
	head, tail *revisionState
	count      uint64
	global     bool
}

func (l *idleList) linkOf(r *revisionState) *idleLink {
	if l.global {
		return &r.poolIdle
	}
	return &r.storeIdle
}

// insert keeps idle handles ordered by earliest recoveryUntil with stable
// identity tie-breaking. Real-clock detachments append at the tail in O(1).
func (l *idleList) insert(r *revisionState) {
	link := l.linkOf(r)
	if link.linked {
		return
	}
	cur := l.tail
	for cur != nil && idleBefore(r, cur) {
		cur = l.linkOf(cur).prev
	}
	if cur == nil {
		link.prev = nil
		link.next = l.head
		if l.head != nil {
			l.linkOf(l.head).prev = r
		} else {
			l.tail = r
		}
		l.head = r
	} else {
		curLink := l.linkOf(cur)
		link.prev = cur
		link.next = curLink.next
		if curLink.next != nil {
			l.linkOf(curLink.next).prev = r
		} else {
			l.tail = r
		}
		curLink.next = r
	}
	link.linked = true
	l.count++
}

func (l *idleList) remove(r *revisionState) {
	link := l.linkOf(r)
	if !link.linked {
		return
	}
	if link.prev != nil {
		l.linkOf(link.prev).next = link.next
	} else {
		l.head = link.next
	}
	if link.next != nil {
		l.linkOf(link.next).prev = link.prev
	} else {
		l.tail = link.prev
	}
	*link = idleLink{}
	l.count--
}

func idleBefore(a, b *revisionState) bool {
	if !a.recoveryUntil.Equal(b.recoveryUntil) {
		return a.recoveryUntil.Before(b.recoveryUntil)
	}
	if a.store.shareLabel != b.store.shareLabel {
		return a.store.shareLabel < b.store.shareLabel
	}
	return bytes.Compare(a.descriptor.FileID().Bytes(), b.descriptor.FileID().Bytes()) < 0
}

// RevisionSession scopes lease and distinct-file budgets for one authenticated
// receiver session while providing a session-bound view onto its RevisionStore.
type RevisionSession struct {
	store       *RevisionStore
	id          SessionID
	limits      CapacityLimits
	leaseLedger *capacityledger.Ledger
	// Multi-session sharing of one physical StableFile means per-session file
	// counts are not additive into the share handle ledger, so this stays standalone.
	fileLedger *capacityledger.Ledger
	pending    int
	closed     bool
}

func (s *RevisionSession) SessionID() SessionID {
	if s == nil {
		return ""
	}
	return s.id
}

func (s *RevisionSession) Snapshot() ScopeSnapshot {
	if s == nil || s.store == nil {
		return ScopeSnapshot{}
	}
	s.store.pool.mu.Lock()
	defer s.store.pool.mu.Unlock()
	return s.snapshotLocked()
}

func (s *RevisionSession) snapshotLocked() ScopeSnapshot {
	return ScopeSnapshot{
		scope:    CapacityScopeSession,
		identity: string(s.id),
		limits:   s.limits,
		used: CapacityUsage{
			StableHandles: s.fileLedger.Snapshot().Used,
			ActiveLeases:  s.leaseLedger.Snapshot().Used,
		},
	}
}

func (s *RevisionSession) Close() error {
	if s == nil || s.store == nil {
		return nil
	}
	pool := s.store.pool
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for s.pending != 0 {
		pool.cond.Wait()
	}
	delete(s.store.sessions, s)
	if s.store.sessionByID[s.id] == s {
		delete(s.store.sessionByID, s.id)
	}
	return errors.Join(s.leaseLedger.Close(), s.fileLedger.Close())
}
