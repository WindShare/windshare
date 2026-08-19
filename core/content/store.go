package content

import (
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

type RevisionStoreConfig struct {
	ShareInstance    catalog.ShareInstance
	ChunkSize        uint32
	Catalog          CatalogNodeSource
	Source           RevisionSource
	ProcessQuota     *QuotaAccount
	ShareQuota       *QuotaAccount
	Clock            Clock
	LeaseIDs         LeaseIDGenerator
	RevisionDeriver  RevisionIdentityDeriver
	MetadataBudget   *RevisionMetadataBudget
	CacheInvalidator CacheInvalidator
	Tracer           RevisionTracer
}

type RevisionStore struct {
	shareInstance   catalog.ShareInstance
	chunkSize       uint32
	catalog         CatalogNodeSource
	source          RevisionSource
	processQuota    *QuotaAccount
	shareQuota      *QuotaAccount
	clock           Clock
	leaseIDs        LeaseIDGenerator
	revisionDeriver RevisionIdentityDeriver
	metadataBudget  *RevisionMetadataBudget
	invalidator     CacheInvalidator
	tracer          RevisionTracer

	mu                       sync.Mutex
	closed                   bool
	revisionAdmissionStopped bool
	revisions                map[catalog.FileID]*revisionState
	readingRevisions         map[*revisionState]struct{}
	leases                   map[LeaseID]*leaseState
	opening                  map[catalog.FileID]*openAttempt
	invalidated              map[revisionIdentity]*revisionMetadataReservation
	leaseTombstones          map[LeaseID]leaseStatus
	leaseOrder               []LeaseID
	leaseCursor              int
	openWG                   sync.WaitGroup
	readWG                   sync.WaitGroup
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
	descriptor     FileRevisionDescriptor
	source         StableFile
	handleQuota    *QuotaReservation
	leases         map[LeaseID]*leaseState
	sessionHandles map[*QuotaAccount]*sessionHandleState
	graceUntil     time.Time
	readers        int
	closePending   bool
	closed         bool
	lifecycle      revisionLifecycle
}

func (r *revisionState) identity() revisionIdentity {
	return revisionIdentity{file: r.descriptor.FileID(), revision: r.descriptor.FileRevision()}
}

type sessionHandleState struct {
	quota  *QuotaReservation
	leases uint64
}

type leaseStatus uint8

const (
	leaseActive leaseStatus = iota
	leaseExpired
	leaseDrifted
)

type leaseState struct {
	lease        RevisionLease
	revision     *revisionState
	quota        *QuotaReservation
	sessionQuota *QuotaAccount
	status       leaseStatus
	createdAt    time.Time
	expiresAt    time.Time
	endedAt      time.Time
}

type revisionCleanup struct {
	source      StableFile
	reservation *QuotaReservation
}

func (c revisionCleanup) run() {
	if c.source != nil {
		// A backend panic must not skip quota release and permanently deny
		// unrelated revisions.
		func() {
			defer func() { _ = recover() }()
			_ = c.source.Close()
		}()
	}
	if c.reservation != nil {
		c.reservation.Release()
	}
}

func NewRevisionStore(config RevisionStoreConfig) (*RevisionStore, error) {
	if config.ShareInstance.IsZero() || config.Catalog == nil || config.Source == nil ||
		config.ProcessQuota == nil || config.ShareQuota == nil || config.RevisionDeriver == nil || config.MetadataBudget == nil {
		return nil, errors.New("revision store requires share identity, catalog, source, process/share quotas, revision deriver, and metadata budget")
	}
	if config.ProcessQuota == config.ShareQuota {
		return nil, errors.New("revision store process and share quotas must be distinct")
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
	return &RevisionStore{
		shareInstance: config.ShareInstance, chunkSize: config.ChunkSize,
		catalog: config.Catalog, source: config.Source, processQuota: config.ProcessQuota, shareQuota: config.ShareQuota,
		clock: config.Clock, leaseIDs: config.LeaseIDs, revisionDeriver: config.RevisionDeriver,
		metadataBudget: config.MetadataBudget, invalidator: config.CacheInvalidator, tracer: config.Tracer,
		revisions: make(map[catalog.FileID]*revisionState), readingRevisions: make(map[*revisionState]struct{}),
		leases: make(map[LeaseID]*leaseState), opening: make(map[catalog.FileID]*openAttempt),
		invalidated: make(map[revisionIdentity]*revisionMetadataReservation), leaseTombstones: make(map[LeaseID]leaseStatus),
	}, nil
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
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	attempts := make([]*openAttempt, 0, len(s.opening))
	admissions := make([]*openAdmission, 0, len(s.opening))
	for _, attempt := range s.opening {
		attempts = append(attempts, attempt)
		if attempt.ownerAdmission != nil {
			admissions = append(admissions, attempt.ownerAdmission)
			attempt.ownerAdmission = nil
		}
	}
	s.opening = make(map[catalog.FileID]*openAttempt)
	cleanups := make([]revisionCleanup, 0, len(s.revisions))
	for _, revision := range s.revisions {
		for _, lease := range revision.leases {
			if lease.status == leaseActive {
				lease.status = leaseExpired
				lease.quota.Release()
				lease.quota = nil
			}
			lease.sessionQuota = nil
		}
		releaseAllSessionHandlesLocked(revision)
		revision.lifecycle = revisionLifecycleReleased
		revision.closePending = true
		if revision.readers == 0 && !revision.closed {
			revision.closed = true
			cleanups = append(cleanups, revisionCleanup{source: revision.source, reservation: revision.handleQuota})
			revision.source = nil
			revision.handleQuota = nil
		}
	}
	s.revisions = make(map[catalog.FileID]*revisionState)
	s.leases = make(map[LeaseID]*leaseState)
	s.mu.Unlock()
	for _, admission := range admissions {
		admission.release()
	}
	for _, attempt := range attempts {
		attempt.cancel()
	}
	s.openWG.Wait()
	s.readWG.Wait()
	for _, cleanup := range cleanups {
		cleanup.run()
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
	return nil
}
