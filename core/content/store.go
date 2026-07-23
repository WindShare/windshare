package content

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

type randomIdentities struct{}

func (randomIdentities) NewFileRevision() (FileRevision, error) {
	var revision FileRevision
	if _, err := rand.Read(revision[:]); err != nil {
		return FileRevision{}, fmt.Errorf("generate file revision identity: %w", err)
	}
	return revision, nil
}

func (randomIdentities) NewLeaseID() (LeaseID, error) {
	var lease LeaseID
	if _, err := rand.Read(lease[:]); err != nil {
		return LeaseID{}, fmt.Errorf("generate revision lease identity: %w", err)
	}
	return lease, nil
}

type RevisionStoreConfig struct {
	ShareInstance    catalog.ShareInstance
	ChunkSize        uint32
	Catalog          CatalogNodeSource
	Source           RevisionSource
	ProcessQuota     *QuotaAccount
	ShareQuota       *QuotaAccount
	Clock            Clock
	IDs              IdentityGenerator
	CacheInvalidator CacheInvalidator
}

type RevisionStore struct {
	shareInstance catalog.ShareInstance
	chunkSize     uint32
	catalog       CatalogNodeSource
	source        RevisionSource
	processQuota  *QuotaAccount
	shareQuota    *QuotaAccount
	clock         Clock
	ids           IdentityGenerator
	invalidator   CacheInvalidator

	mu              sync.Mutex
	closed          bool
	revisions       map[catalog.FileID]*revisionState
	leases          map[LeaseID]*leaseState
	opening         map[catalog.FileID]*openAttempt
	usedRevisions   map[FileRevision]struct{}
	leaseTombstones map[LeaseID]leaseStatus
	revisionOrder   []FileRevision
	leaseOrder      []LeaseID
	revisionCursor  int
	leaseCursor     int
	openWG          sync.WaitGroup
	readWG          sync.WaitGroup
}

const IdentityTombstoneLimit = 4_096

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
	drifted        bool
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

type openAttempt struct {
	file           catalog.FileID
	done           chan struct{}
	cancel         context.CancelFunc
	waiters        int
	completed      bool
	ownerAdmission *openAdmission
	revision       *revisionState
	err            error
}

type openAdmission struct {
	leaseQuota         *QuotaReservation
	sessionHandleQuota *QuotaReservation
}

func (a *openAdmission) release() {
	if a == nil {
		return
	}
	a.leaseQuota.Release()
	a.leaseQuota = nil
	a.sessionHandleQuota.Release()
	a.sessionHandleQuota = nil
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
	if config.ShareInstance.IsZero() || config.Catalog == nil || config.Source == nil || config.ProcessQuota == nil || config.ShareQuota == nil {
		return nil, errors.New("revision store requires share identity, catalog, source, and process/share quotas")
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
	if config.IDs == nil {
		config.IDs = randomIdentities{}
	}
	return &RevisionStore{
		shareInstance: config.ShareInstance, chunkSize: config.ChunkSize,
		catalog: config.Catalog, source: config.Source, processQuota: config.ProcessQuota, shareQuota: config.ShareQuota,
		clock: config.Clock, ids: config.IDs, invalidator: config.CacheInvalidator,
		revisions: make(map[catalog.FileID]*revisionState), leases: make(map[LeaseID]*leaseState), opening: make(map[catalog.FileID]*openAttempt),
		usedRevisions: make(map[FileRevision]struct{}), leaseTombstones: make(map[LeaseID]leaseStatus),
	}, nil
}

func (s *RevisionStore) OpenRevision(ctx context.Context, file catalog.FileID, sessionQuota *QuotaAccount) (RevisionLease, error) {
	if err := ctx.Err(); err != nil {
		return RevisionLease{}, err
	}
	if file.IsZero() || sessionQuota == nil {
		return RevisionLease{}, errors.New("open revision requires a file identity and session quota")
	}
	s.reap(s.clock.Now())

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return RevisionLease{}, ErrRevisionStoreClosed
	}
	if revision := s.revisions[file]; revision != nil && !revision.drifted && !revision.closed {
		lease, err := s.acquireLeaseLocked(revision, sessionQuota, s.clock.Now(), nil)
		s.mu.Unlock()
		return lease, err
	}
	if attempt := s.opening[file]; attempt != nil {
		attempt.waiters++
		s.mu.Unlock()
		return s.awaitOpen(ctx, attempt, sessionQuota, false)
	}
	admission, err := s.reserveOpenAdmission(sessionQuota)
	if err != nil {
		s.mu.Unlock()
		return RevisionLease{}, err
	}
	attemptContext, cancel := context.WithCancel(context.Background())
	attempt := &openAttempt{file: file, done: make(chan struct{}), cancel: cancel, waiters: 1, ownerAdmission: admission}
	s.opening[file] = attempt
	s.openWG.Add(1)
	s.mu.Unlock()
	go s.runOpen(attemptContext, attempt)
	return s.awaitOpen(ctx, attempt, sessionQuota, true)
}

func (s *RevisionStore) reserveOpenAdmission(sessionQuota *QuotaAccount) (*openAdmission, error) {
	leaseQuota, err := ReserveQuota(QuotaHierarchy{Process: s.processQuota, Share: s.shareQuota, Session: sessionQuota}, QuotaUsage{ActiveLeases: 1})
	if err != nil {
		return nil, err
	}
	handleQuota, err := reserveQuotaAccounts([]*QuotaAccount{sessionQuota}, QuotaUsage{StableHandles: 1})
	if err != nil {
		leaseQuota.Release()
		return nil, err
	}
	return &openAdmission{leaseQuota: leaseQuota, sessionHandleQuota: handleQuota}, nil
}

func (s *RevisionStore) acquireLeaseLocked(revision *revisionState, sessionQuota *QuotaAccount, now time.Time, admission *openAdmission) (RevisionLease, error) {
	defer admission.release()
	var leaseQuota *QuotaReservation
	if admission != nil {
		leaseQuota = admission.leaseQuota
		admission.leaseQuota = nil
	}
	if leaseQuota == nil {
		var err error
		leaseQuota, err = ReserveQuota(QuotaHierarchy{Process: s.processQuota, Share: s.shareQuota, Session: sessionQuota}, QuotaUsage{ActiveLeases: 1})
		if err != nil {
			return RevisionLease{}, err
		}
	}
	sessionHandle := revision.sessionHandles[sessionQuota]
	newSessionHandle := sessionHandle == nil
	if newSessionHandle {
		var handleQuota *QuotaReservation
		if admission != nil {
			handleQuota = admission.sessionHandleQuota
			admission.sessionHandleQuota = nil
		}
		if handleQuota == nil {
			var reserveErr error
			handleQuota, reserveErr = reserveQuotaAccounts([]*QuotaAccount{sessionQuota}, QuotaUsage{StableHandles: 1})
			if reserveErr != nil {
				leaseQuota.Release()
				return RevisionLease{}, reserveErr
			}
		}
		sessionHandle = &sessionHandleState{quota: handleQuota}
	}
	leaseID, err := s.ids.NewLeaseID()
	if err != nil {
		leaseQuota.Release()
		if newSessionHandle {
			sessionHandle.quota.Release()
		}
		return RevisionLease{}, err
	}
	if leaseID.IsZero() {
		leaseQuota.Release()
		if newSessionHandle {
			sessionHandle.quota.Release()
		}
		return RevisionLease{}, errors.New("revision lease generator returned a zero identity")
	}
	if _, exists := s.leases[leaseID]; exists {
		leaseQuota.Release()
		if newSessionHandle {
			sessionHandle.quota.Release()
		}
		return RevisionLease{}, errors.New("revision lease generator reused an identity")
	}
	if _, exists := s.leaseTombstones[leaseID]; exists {
		leaseQuota.Release()
		if newSessionHandle {
			sessionHandle.quota.Release()
		}
		return RevisionLease{}, errors.New("revision lease generator reused an identity")
	}
	lease := RevisionLease{id: leaseID, descriptor: revision.descriptor, ttl: LeaseTTL, renewAfter: LeaseTTL - LeaseRenewWindow}
	state := &leaseState{
		lease: lease, revision: revision, quota: leaseQuota, sessionQuota: sessionQuota, status: leaseActive,
		createdAt: now, expiresAt: now.Add(LeaseTTL),
	}
	if newSessionHandle {
		revision.sessionHandles[sessionQuota] = sessionHandle
	}
	sessionHandle.leases++
	revision.leases[leaseID] = state
	revision.graceUntil = time.Time{}
	s.leases[leaseID] = state
	return lease, nil
}

func (s *RevisionStore) awaitOpen(ctx context.Context, attempt *openAttempt, sessionQuota *QuotaAccount, ownsAdmission bool) (RevisionLease, error) {
	select {
	case <-attempt.done:
		s.mu.Lock()
		attempt.waiters--
		var admission *openAdmission
		if ownsAdmission {
			admission = attempt.ownerAdmission
			attempt.ownerAdmission = nil
			if s.opening[attempt.file] == attempt {
				delete(s.opening, attempt.file)
			}
		}
		if attempt.err != nil {
			err := attempt.err
			s.mu.Unlock()
			admission.release()
			return RevisionLease{}, err
		}
		if s.closed || attempt.revision == nil || attempt.revision.closed {
			s.mu.Unlock()
			admission.release()
			return RevisionLease{}, ErrRevisionStoreClosed
		}
		lease, err := s.acquireLeaseLocked(attempt.revision, sessionQuota, s.clock.Now(), admission)
		s.mu.Unlock()
		return lease, err
	case <-ctx.Done():
		s.mu.Lock()
		var admission *openAdmission
		if ownsAdmission {
			admission = attempt.ownerAdmission
			attempt.ownerAdmission = nil
			if attempt.completed && s.opening[attempt.file] == attempt {
				delete(s.opening, attempt.file)
			}
		}
		if !attempt.completed {
			attempt.waiters--
			if attempt.waiters == 0 {
				if s.opening[attempt.file] == attempt {
					delete(s.opening, attempt.file)
				}
				attempt.cancel()
			}
		}
		s.mu.Unlock()
		admission.release()
		return RevisionLease{}, ctx.Err()
	}
}

func (s *RevisionStore) runOpen(ctx context.Context, attempt *openAttempt) {
	defer s.openWG.Done()
	var revision *revisionState
	var resultErr error
	defer func() {
		if recovered := recover(); recovered != nil {
			resultErr = fmt.Errorf("revision source panicked: %v\n%s", recovered, debug.Stack())
		}
		s.completeOpen(attempt, revision, resultErr)
	}()
	record, exists, err := s.catalog.Node(ctx, attempt.file.NodeID())
	if err != nil {
		resultErr = err
		return
	}
	recordFile, isFile := record.FileID()
	if !exists || !isFile || recordFile != attempt.file {
		resultErr = ErrRevisionNotFound
		return
	}
	// The physical source is share-scoped and survives ProtocolSession reconnects;
	// each session is charged separately only while it owns a lease below.
	handleQuota, err := reserveQuotaAccounts([]*QuotaAccount{s.processQuota, s.shareQuota}, QuotaUsage{StableHandles: 1})
	if err != nil {
		resultErr = err
		return
	}
	var stable StableFile
	cleanup := true
	defer func() {
		if cleanup {
			revisionCleanup{source: stable, reservation: handleQuota}.run()
		}
	}()
	stable, err = s.source.OpenStable(ctx, record)
	if err != nil {
		resultErr = err
		return
	}
	if err := stable.Verify(ctx); err != nil {
		if errors.Is(err, ErrSourceDrift) {
			resultErr = ErrRevisionStale
		} else {
			resultErr = fmt.Errorf("verify stable file before revision publication: %w", err)
		}
		return
	}
	expectedSize := record.Entry().ExpectedSize()
	if stable.ExactSize() != expectedSize {
		resultErr = ErrRevisionStale
		return
	}
	geometry, err := NewFileGeometry(stable.ExactSize(), s.chunkSize)
	if err != nil {
		resultErr = err
		return
	}
	revisionID, err := s.ids.NewFileRevision()
	if err != nil {
		resultErr = err
		return
	}
	if revisionID.IsZero() {
		resultErr = errors.New("file revision generator returned a zero identity")
		return
	}
	descriptor, err := NewFileRevisionDescriptor(s.shareInstance, attempt.file, revisionID, geometry, stable.ModifiedTime())
	if err != nil {
		resultErr = err
		return
	}
	revision = &revisionState{
		descriptor: descriptor, source: stable, handleQuota: handleQuota,
		leases: make(map[LeaseID]*leaseState), sessionHandles: make(map[*QuotaAccount]*sessionHandleState),
		graceUntil: s.clock.Now().Add(RevisionResumeGrace),
	}
	cleanup = false
}

func (s *RevisionStore) completeOpen(attempt *openAttempt, revision *revisionState, resultErr error) {
	var cleanup revisionCleanup
	var admission *openAdmission
	s.mu.Lock()
	if attempt.completed {
		s.mu.Unlock()
		return
	}
	attempt.completed = true
	current := s.opening[attempt.file] == attempt
	if resultErr == nil && revision != nil && current && !s.closed {
		if _, reused := s.usedRevisions[revision.descriptor.FileRevision()]; reused {
			resultErr = errors.New("file revision generator reused an identity")
		} else if existing := s.revisions[attempt.file]; existing != nil {
			resultErr = errors.New("revision source raced an existing file revision")
		} else {
			s.rememberRevisionIDLocked(revision.descriptor.FileRevision())
			s.revisions[attempt.file] = revision
			attempt.revision = revision
		}
	}
	if resultErr == nil && attempt.revision == nil {
		resultErr = context.Canceled
	}
	attempt.err = resultErr
	if resultErr != nil || attempt.revision == nil {
		admission = attempt.ownerAdmission
		attempt.ownerAdmission = nil
	}
	if current && attempt.ownerAdmission == nil {
		delete(s.opening, attempt.file)
	}
	if revision != nil && attempt.revision != revision {
		revision.closed = true
		cleanup = revisionCleanup{source: revision.source, reservation: revision.handleQuota}
	}
	s.mu.Unlock()
	cleanup.run()
	admission.release()
	// Closing done publishes both the result and completed rollback, so callers
	// never observe a failed open with admission still charged.
	close(attempt.done)
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

func (s *RevisionStore) rememberRevisionIDLocked(id FileRevision) {
	if len(s.revisionOrder) < IdentityTombstoneLimit {
		s.revisionOrder = append(s.revisionOrder, id)
	} else {
		delete(s.usedRevisions, s.revisionOrder[s.revisionCursor])
		s.revisionOrder[s.revisionCursor] = id
		s.revisionCursor = (s.revisionCursor + 1) % IdentityTombstoneLimit
	}
	s.usedRevisions[id] = struct{}{}
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
		revision.closePending = true
		if revision.readers == 0 && !revision.closed {
			revision.closed = true
			cleanups = append(cleanups, revisionCleanup{source: revision.source, reservation: revision.handleQuota})
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
	return nil
}
