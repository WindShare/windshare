package content

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
)

type RevisionStoreConfig struct {
	ShareInstance       catalog.ShareInstance
	ChunkSize           uint32
	Catalog             CatalogNodeSource
	Source              RevisionSource
	CapacityCoordinator *revisioncapacity.Coordinator
	CapacityStore       revisioncapacity.StoreConfig
	Clock               Clock
	LeaseIDs            LeaseIDGenerator
	RevisionDeriver     RevisionIdentityDeriver
	MetadataBudget      *RevisionMetadataBudget
	CacheInvalidator    CacheInvalidator
	Tracer              RevisionTracer
}

type RevisionStore struct {
	shareInstance    catalog.ShareInstance
	chunkSize        uint32
	catalog          CatalogNodeSource
	source           RevisionSource
	capacity         *revisioncapacity.StoreRegistration
	capacitySessions map[*revisioncapacity.SessionRegistration]struct{}
	capacityContext  context.Context
	capacityCancel   context.CancelFunc
	clock            Clock
	leaseIDs         LeaseIDGenerator
	revisionDeriver  RevisionIdentityDeriver
	metadataBudget   *RevisionMetadataBudget
	invalidator      CacheInvalidator
	tracer           RevisionTracer

	mu                       sync.Mutex
	closed                   bool
	revisionAdmissionStopped bool
	revisions                map[catalog.FileID]*revisionState
	readingRevisions         map[*revisionState]struct{}
	leases                   map[LeaseID]*leaseState
	opening                  map[catalog.FileID]*openAttempt
	idleRevisions            map[revisioncapacity.CandidateToken]*revisionState
	invalidated              map[revisionIdentity]*revisionMetadataReservation
	leaseTombstones          map[LeaseID]leaseStatus
	leaseOrder               []LeaseID
	leaseCursor              int
	openWG                   sync.WaitGroup
	readWG                   sync.WaitGroup
	capacityWG               sync.WaitGroup
	closeMu                  sync.Mutex
	localCloseComplete       bool
	localCloseErr            error
	capacityCloseComplete    bool
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

func (i revisionIdentity) isZero() bool { return i.file.IsZero() || i.revision.IsZero() }

type revisionState struct {
	descriptor          FileRevisionDescriptor
	source              StableFile
	handleCharge        revisioncapacity.StableHandleCharge
	leases              map[LeaseID]*leaseState
	activeLeases        uint64
	sessionHandles      map[*revisioncapacity.SessionRegistration]*sessionHandleState
	recoveryUntil       time.Time
	lifecycleGeneration uint64
	idleToken           revisioncapacity.CandidateToken
	admissionDone       chan struct{}
	readers             int
	closePending        bool
	closing             bool
	closed              bool
	lifecycle           revisionLifecycle
}

func (r *revisionState) identity() revisionIdentity {
	return revisionIdentity{file: r.descriptor.FileID(), revision: r.descriptor.FileRevision()}
}

type sessionHandleState struct {
	charge revisioncapacity.SessionHandleCharge
	leases uint64
}

type leaseStatus uint8

const (
	leaseActive leaseStatus = iota
	leaseEnded
	leaseDrifted
)

type leaseState struct {
	lease        RevisionLease
	revision     *revisionState
	activeCharge revisioncapacity.ActiveLeaseCharge
	session      *revisioncapacity.SessionRegistration
	status       leaseStatus
	createdAt    time.Time
	expiresAt    time.Time
	endedAt      time.Time
}

type revisionCleanup struct {
	store        *RevisionStore
	revision     *revisionState
	idleToken    revisioncapacity.CandidateToken
	source       StableFile
	stableCharge revisioncapacity.StableHandleCharge
}

type stableClosePanicError struct {
	recovered any
	stack     []byte
}

func (e *stableClosePanicError) Error() string {
	return fmt.Sprintf("stable file close panicked: %v\n%s", e.recovered, e.stack)
}

func (c revisionCleanup) run() (err error) {
	if c.idleToken != "" && c.store != nil && c.revision != nil {
		c.store.capacity.WithdrawIdle(c.idleToken)
		if waitErr := c.store.capacity.WaitForReclaims(); waitErr != nil {
			return waitErr
		}
		c.store.mu.Lock()
		if c.revision.source != nil && !c.revision.closing && !c.revision.closed {
			c.revision.closing = true
			c.source = c.revision.source
			c.stableCharge = c.revision.handleCharge
			c.revision.source = nil
			c.revision.handleCharge = revisioncapacity.StableHandleCharge{}
		}
		c.store.mu.Unlock()
	}
	terminal := c.source == nil
	if c.source != nil {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					err = &stableClosePanicError{recovered: recovered, stack: debug.Stack()}
				}
			}()
			err = c.source.Close()
			terminal = true
		}()
	}
	if c.stableCharge.Valid() {
		if terminal {
			err = errors.Join(err, c.stableCharge.Release())
		} else {
			err = errors.Join(err, c.stableCharge.Quarantine(err))
		}
	}
	if c.store != nil && c.revision != nil {
		var panicErr *stableClosePanicError
		c.store.mu.Lock()
		c.revision.closing = false
		c.revision.closed = !errors.As(err, &panicErr)
		c.store.mu.Unlock()
	}
	return err
}

func NewRevisionStore(config RevisionStoreConfig) (*RevisionStore, error) {
	if config.ShareInstance.IsZero() || config.Catalog == nil || config.Source == nil ||
		config.CapacityCoordinator == nil || config.RevisionDeriver == nil || config.MetadataBudget == nil {
		return nil, errors.New("revision store requires share identity, catalog, source, capacity coordinator, revision deriver, and metadata budget")
	}
	if _, err := NewFileGeometry(0, config.ChunkSize); err != nil {
		return nil, err
	}
	if config.Clock == nil {
		config.Clock = wallClock{}
	}
	if config.LeaseIDs == nil {
		config.LeaseIDs = randomLeaseIDs{}
	}
	capacityContext, capacityCancel := context.WithCancel(context.Background())
	store := &RevisionStore{
		shareInstance: config.ShareInstance, chunkSize: config.ChunkSize,
		catalog: config.Catalog, source: config.Source,
		clock: config.Clock, leaseIDs: config.LeaseIDs, revisionDeriver: config.RevisionDeriver,
		metadataBudget: config.MetadataBudget, invalidator: config.CacheInvalidator, tracer: config.Tracer,
		capacityContext: capacityContext, capacityCancel: capacityCancel,
		capacitySessions: make(map[*revisioncapacity.SessionRegistration]struct{}),
		revisions:        make(map[catalog.FileID]*revisionState), readingRevisions: make(map[*revisionState]struct{}),
		leases: make(map[LeaseID]*leaseState), opening: make(map[catalog.FileID]*openAttempt),
		idleRevisions: make(map[revisioncapacity.CandidateToken]*revisionState),
		invalidated:   make(map[revisionIdentity]*revisionMetadataReservation), leaseTombstones: make(map[LeaseID]leaseStatus),
	}
	registration, err := config.CapacityCoordinator.RegisterStore(config.CapacityStore, store)
	if err != nil {
		capacityCancel()
		return nil, fmt.Errorf("register revision store capacity: %w", err)
	}
	store.capacity = registration
	return store, nil
}

func (s *RevisionStore) RegisterSession(config revisioncapacity.SessionConfig) (*revisioncapacity.SessionRegistration, error) {
	if s == nil || s.capacity == nil {
		return nil, errors.New("revision store has no capacity registration")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrRevisionStoreClosed
	}
	// Add while holding s.mu so Close cannot observe closed=true and begin its
	// wait before this registration rollback is part of the joined lifecycle.
	s.capacityWG.Add(1)
	s.mu.Unlock()
	defer s.capacityWG.Done()
	session, err := s.capacity.RegisterSession(config)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = session.Close()
		return nil, ErrRevisionStoreClosed
	}
	s.capacitySessions[session] = struct{}{}
	s.mu.Unlock()
	return session, nil
}

func (s *RevisionStore) CapacitySnapshot() revisioncapacity.CapacitySnapshot {
	if s == nil || s.capacity == nil {
		return revisioncapacity.CapacitySnapshot{}
	}
	return s.capacity.Snapshot()
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
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if !s.localCloseComplete {
		s.localCloseErr = s.closeLocal()
		s.localCloseComplete = true
	}
	if s.capacityCloseComplete || s.capacity == nil {
		return s.localCloseErr
	}
	capacityErr := s.capacity.Close()
	if capacityErr == nil {
		s.capacityCloseComplete = true
	}
	return errors.Join(s.localCloseErr, capacityErr)
}

func (s *RevisionStore) closeLocal() error {
	s.mu.Lock()
	s.closed = true
	s.capacityCancel()
	attempts := make([]*openAttempt, 0, len(s.opening))
	for _, attempt := range s.opening {
		attempts = append(attempts, attempt)
	}
	idleTokens := make([]revisioncapacity.CandidateToken, 0, len(s.revisions))
	chargeReleases := make([]capacityChargeRelease, 0)
	for _, revision := range s.revisions {
		revision.lifecycleGeneration++
		if revision.idleToken != "" {
			idleTokens = append(idleTokens, revision.idleToken)
			delete(s.idleRevisions, revision.idleToken)
			revision.idleToken = ""
		}
		for _, lease := range revision.leases {
			if lease.status == leaseActive {
				lease.status = leaseEnded
				chargeReleases = append(chargeReleases, capacityChargeRelease{active: lease.activeCharge})
				lease.activeCharge = revisioncapacity.ActiveLeaseCharge{}
			}
			lease.session = nil
		}
		revision.activeLeases = 0
		chargeReleases = append(chargeReleases, releaseAllSessionHandlesLocked(revision)...)
		revision.lifecycle = revisionLifecycleReleased
		revision.closePending = true
	}
	s.leases = make(map[LeaseID]*leaseState)
	s.mu.Unlock()
	for _, token := range idleTokens {
		s.capacity.WithdrawIdle(token)
	}
	for _, attempt := range attempts {
		attempt.cancel()
	}
	var closeErr error
	for _, release := range chargeReleases {
		closeErr = errors.Join(closeErr, release.run())
	}
	s.openWG.Wait()
	s.capacityWG.Wait()
	s.readWG.Wait()
	closeErr = errors.Join(closeErr, s.capacity.WaitForReclaims())
	s.mu.Lock()
	cleanups := make([]revisionCleanup, 0, len(s.revisions))
	for _, revision := range s.revisions {
		if revision.source != nil && !revision.closing && !revision.closed {
			revision.closing = true
			cleanups = append(cleanups, revisionCleanup{store: s, revision: revision, source: revision.source, stableCharge: revision.handleCharge})
			revision.source = nil
			revision.handleCharge = revisioncapacity.StableHandleCharge{}
		}
	}
	s.revisions = make(map[catalog.FileID]*revisionState)
	s.mu.Unlock()
	for _, cleanup := range cleanups {
		closeErr = errors.Join(closeErr, cleanup.run())
	}
	s.mu.Lock()
	sessions := make([]*revisioncapacity.SessionRegistration, 0, len(s.capacitySessions))
	for session := range s.capacitySessions {
		sessions = append(sessions, session)
	}
	s.capacitySessions = make(map[*revisioncapacity.SessionRegistration]struct{})
	s.mu.Unlock()
	for _, session := range sessions {
		closeErr = errors.Join(closeErr, session.Close())
	}
	s.mu.Lock()
	metadata := make([]*revisionMetadataReservation, 0, len(s.invalidated))
	for _, reservation := range s.invalidated {
		metadata = append(metadata, reservation)
	}
	s.invalidated = make(map[revisionIdentity]*revisionMetadataReservation)
	s.mu.Unlock()
	for _, reservation := range metadata {
		reservation.release()
	}
	return closeErr
}
