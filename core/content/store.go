package content

import (
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/windshare/windshare/core/capacityledger"
	"github.com/windshare/windshare/core/catalog"
)

type RevisionStoreConfig struct {
	ShareInstance  catalog.ShareInstance
	ChunkSize      uint32
	Catalog        CatalogNodeSource
	Source         RevisionSource
	HandlePool     *HandlePool
	ShareLimits    CapacityLimits
	CapacityTracer CapacityTracer
	Clock          Clock
	RevisionIDs    RevisionIDGenerator
	LeaseIDs       LeaseIDGenerator
	Cache          RevisionCache
	Tracer         RevisionTracer
}

type RevisionStore struct {
	shareInstance  catalog.ShareInstance
	chunkSize      uint32
	catalog        CatalogNodeSource
	source         RevisionSource
	pool           *HandlePool
	shareLabel     string
	shareLimits    CapacityLimits
	handleLedger   *capacityledger.Ledger
	leaseLedger    *capacityledger.Ledger
	capacityTracer CapacityTracer
	clock          Clock
	revisionIDs    RevisionIDGenerator
	leaseIDs       LeaseIDGenerator
	cache          RevisionCache
	tracer         RevisionTracer

	// All mutable fields below are serialized by pool.mu.
	closed           bool
	revisions        map[catalog.FileID]*revisionState
	readingRevisions map[*revisionState]struct{}
	leases           map[LeaseID]*leaseState
	opening          map[catalog.FileID]*openAttempt
	idle             idleList
	sessions         map[*RevisionSession]struct{}
	sessionByID      map[SessionID]*RevisionSession
	leaseTombstones  map[LeaseID]leaseStatus
	leaseOrder       []LeaseID
	leaseCursor      int

	openWG    sync.WaitGroup
	readWG    sync.WaitGroup
	closeWG   sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

const IdentityTombstoneLimit = 4_096

type revisionLifecycle uint8

const (
	revisionLifecycleUnknown revisionLifecycle = iota
	revisionLifecycleActive
	revisionLifecycleReleased
	revisionLifecycleInvalidated
)

type revisionIdentity struct {
	file     catalog.FileID
	revision FileRevision
}

type revisionState struct {
	store          *RevisionStore
	descriptor     FileRevisionDescriptor
	source         StableFile
	handleHold     capacityledger.Hold
	leases         map[LeaseID]*leaseState
	activeLeases   uint64
	sessionHandles map[*RevisionSession]*sessionHandleState
	recoveryUntil  time.Time
	poolIdle       idleLink
	storeIdle      idleLink
	readers        int
	closePending   bool
	closing        bool
	closed         bool
	lifecycle      revisionLifecycle
}

func (r *revisionState) identity() revisionIdentity {
	return revisionIdentity{file: r.descriptor.FileID(), revision: r.descriptor.FileRevision()}
}

type sessionHandleState struct {
	hold   capacityledger.Hold
	leases uint64
}

type leaseStatus uint8

const (
	leaseActive leaseStatus = iota
	leaseEnded
	leaseDrifted
)

type leaseState struct {
	lease     RevisionLease
	revision  *revisionState
	leaseHold capacityledger.Hold
	session   *RevisionSession
	status    leaseStatus
	createdAt time.Time
	expiresAt time.Time
	endedAt   time.Time
}

type revisionCleanup struct {
	store      *RevisionStore
	revision   *revisionState
	source     StableFile
	handleHold capacityledger.Hold
}

func closeStableFile(source StableFile) (err error) {
	if source == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("stable file close panicked: %v\n%s", recovered, debug.Stack())
		}
	}()
	return source.Close()
}

func (c revisionCleanup) run() error {
	if c.source == nil && c.handleHold.Units() == 0 {
		return nil
	}
	err := closeStableFile(c.source)
	if c.store != nil {
		c.store.pool.mu.Lock()
		c.handleHold.Release()
		if c.revision != nil {
			c.revision.closing = false
			c.revision.closed = true
		}
		c.store.pool.mu.Unlock()
	}
	return err
}

func NewRevisionStore(config RevisionStoreConfig) (*RevisionStore, error) {
	if config.ShareInstance.IsZero() || config.Catalog == nil || config.Source == nil || config.HandlePool == nil {
		return nil, errors.New("revision store requires share identity, catalog, source, and handle pool")
	}
	if _, err := NewFileGeometry(0, config.ChunkSize); err != nil {
		return nil, err
	}
	limits := config.ShareLimits
	if limits == (CapacityLimits{}) {
		limits = DefaultShareCapacityLimits()
	}
	if config.Clock == nil {
		config.Clock = wallClock{}
	}
	if config.RevisionIDs == nil {
		config.RevisionIDs = randomRevisionIDs{}
	}
	if config.LeaseIDs == nil {
		config.LeaseIDs = randomLeaseIDs{}
	}
	store := &RevisionStore{
		shareInstance:    config.ShareInstance,
		chunkSize:        config.ChunkSize,
		catalog:          config.Catalog,
		source:           config.Source,
		pool:             config.HandlePool,
		capacityTracer:   config.CapacityTracer,
		clock:            config.Clock,
		revisionIDs:      config.RevisionIDs,
		leaseIDs:         config.LeaseIDs,
		cache:            config.Cache,
		tracer:           config.Tracer,
		revisions:        make(map[catalog.FileID]*revisionState),
		readingRevisions: make(map[*revisionState]struct{}),
		leases:           make(map[LeaseID]*leaseState),
		opening:          make(map[catalog.FileID]*openAttempt),
		sessions:         make(map[*RevisionSession]struct{}),
		sessionByID:      make(map[SessionID]*RevisionSession),
		leaseTombstones:  make(map[LeaseID]leaseStatus),
	}
	if err := config.HandlePool.registerStore(store, limits); err != nil {
		return nil, fmt.Errorf("register revision store handle pool: %w", err)
	}
	return store, nil
}

func (s *RevisionStore) RegisterSession(config SessionConfig) (*RevisionSession, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("revision store has no handle pool")
	}
	if config.SessionID == "" {
		return nil, errors.New("revision session registration requires a session identity")
	}
	if err := validateCapacityLimits(config.Limits); err != nil {
		return nil, err
	}
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	if s.pool.closed {
		return nil, ErrHandlePoolClosed
	}
	if s.closed {
		return nil, ErrRevisionStoreClosed
	}
	if _, exists := s.sessionByID[config.SessionID]; exists {
		return nil, fmt.Errorf("revision session identity %q is already registered", config.SessionID)
	}
	session := &RevisionSession{
		store:       s,
		id:          config.SessionID,
		limits:      config.Limits,
		leaseLedger: s.leaseLedger.Child(config.Limits.ActiveLeases),
		fileLedger:  capacityledger.New(config.Limits.StableHandles),
	}
	s.sessions[session] = struct{}{}
	s.sessionByID[session.id] = session
	return session, nil
}

func (s *RevisionStore) CapacitySnapshot() CapacitySnapshot {
	if s == nil || s.pool == nil {
		return CapacitySnapshot{}
	}
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	return s.pool.storeSnapshotLocked(s, true, nil)
}

func (s *RevisionStore) rememberLeaseTombstoneLocked(id LeaseID, status leaseStatus) {
	if len(s.leaseOrder) < IdentityTombstoneLimit {
		s.leaseOrder = append(s.leaseOrder, id)
	} else {
		delete(s.leaseTombstones, s.leaseOrder[s.leaseCursor])
		s.leaseOrder[s.leaseCursor] = id
		s.leaseCursor = (s.leaseCursor + 1) % IdentityTombstoneLimit
	}
	s.leaseTombstones[id] = status
}

func (s *RevisionStore) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closeErr = s.closeStore()
	})
	return s.closeErr
}

func (s *RevisionStore) closeStore() error {
	s.pool.mu.Lock()
	s.closed = true
	attempts := make([]*openAttempt, 0, len(s.opening))
	for _, attempt := range s.opening {
		attempts = append(attempts, attempt)
	}
	retiredIdentities := make([]revisionIdentity, 0, len(s.revisions))
	for _, revision := range s.revisions {
		if revision.lifecycle == revisionLifecycleActive {
			retiredIdentities = append(retiredIdentities, revision.identity())
		}
		s.pool.unlinkIdleLocked(revision)
		for _, lease := range revision.leases {
			if lease.status == leaseActive {
				lease.status = leaseEnded
				lease.leaseHold.Release()
				lease.leaseHold = capacityledger.Hold{}
			}
			lease.session = nil
		}
		revision.activeLeases = 0
		releaseAllSessionHandlesLocked(revision)
		revision.lifecycle = revisionLifecycleReleased
		revision.closePending = true
	}
	s.leases = make(map[LeaseID]*leaseState)
	s.pool.mu.Unlock()

	for _, attempt := range attempts {
		attempt.cancel()
	}
	for _, identity := range retiredIdentities {
		s.retireRevisionCache(identity)
	}
	s.openWG.Wait()
	s.readWG.Wait()
	s.closeWG.Wait()

	s.pool.mu.Lock()
	cleanups := make([]revisionCleanup, 0, len(s.revisions))
	for _, revision := range s.revisions {
		if revision.source != nil && !revision.closing && !revision.closed {
			revision.closing = true
			cleanups = append(cleanups, revisionCleanup{store: s, revision: revision, source: revision.source, handleHold: revision.handleHold})
			revision.source = nil
			revision.handleHold = capacityledger.Hold{}
		}
	}
	s.revisions = make(map[catalog.FileID]*revisionState)
	s.pool.mu.Unlock()

	var closeErr error
	for _, cleanup := range cleanups {
		closeErr = errors.Join(closeErr, cleanup.run())
	}

	s.pool.mu.Lock()
	for session := range s.sessions {
		session.closed = true
		delete(s.sessions, session)
		delete(s.sessionByID, session.id)
		closeErr = errors.Join(closeErr, session.leaseLedger.Close(), session.fileLedger.Close())
	}
	closeErr = errors.Join(closeErr, s.handleLedger.Close(), s.leaseLedger.Close())
	delete(s.pool.stores, s)
	s.pool.cond.Broadcast()
	s.pool.mu.Unlock()
	return closeErr
}
