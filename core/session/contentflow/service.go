package contentflow

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const ReleasedLeaseTombstoneLimit = 4_096

var errRevisionOperationScope = errors.New("content failure belongs to the file revision")

type RevisionStore interface {
	OpenRevisions(context.Context, []content.OpenRevisionRequest, *content.QuotaAccount) ([]content.OpenRevisionResult, error)
	RenewLease(content.LeaseID) (content.RevisionLease, error)
	ReleaseLease(content.LeaseID) error
	ValidateLease(content.LeaseID, content.FileRevisionDescriptor) error
	ReadBlock(context.Context, content.LeaseID, content.BlockRef) ([]byte, error)
}

type RecordSealer interface {
	SealRevision(content.FileRevisionDescriptor) ([]byte, error)
	SealBlock(records.BlockRecord) (records.SealedBlock, error)
}

type SenderServiceConfig struct {
	Store        RevisionStore
	SessionQuota *content.QuotaAccount
	Sealer       RecordSealer
	Cache        *SharedBlockCache
}

type SenderService struct {
	store        RevisionStore
	sessionQuota *content.QuotaAccount
	sealer       RecordSealer
	cache        *SharedBlockCache

	mu             sync.Mutex
	closed         bool
	leases         map[content.LeaseID]content.FileRevisionDescriptor
	released       map[content.LeaseID]struct{}
	releasedOrder  []content.LeaseID
	releasedCursor int
}

func NewSenderService(config SenderServiceConfig) (*SenderService, error) {
	if config.Store == nil || config.SessionQuota == nil || config.Sealer == nil || config.Cache == nil {
		return nil, errors.New("content sender service requires store, session quota, sealer, and share cache")
	}
	return &SenderService{
		store: config.Store, sessionQuota: config.SessionQuota, sealer: config.Sealer, cache: config.Cache,
		leases: make(map[content.LeaseID]content.FileRevisionDescriptor), released: make(map[content.LeaseID]struct{}),
	}, nil
}

func (s *SenderService) Open(ctx context.Context, request OpenRequest) (OpenResults, error) {
	validated, err := NewOpenRequest(request.Items())
	if err != nil {
		return OpenResults{}, err
	}
	if err := s.checkOpen(); err != nil {
		return OpenResults{}, err
	}
	storeRequests := make([]content.OpenRevisionRequest, len(validated.items))
	for index, item := range validated.items {
		storeRequests[index] = content.OpenRevisionRequest{FileID: item.FileID, InitialRanges: item.InitialRanges}
	}
	storeResults, err := s.store.OpenRevisions(ctx, storeRequests, s.sessionQuota)
	if err != nil {
		s.releaseStoreResults(storeResults)
		return OpenResults{}, err
	}
	if len(storeResults) != len(validated.items) {
		s.releaseStoreResults(storeResults)
		return OpenResults{}, fmt.Errorf("%w: wrong open result count", ErrRevisionStoreContract)
	}
	results, leases, err := s.prepareOpenResults(validated.items, storeResults)
	if err != nil {
		s.releaseStoreResults(storeResults)
		return OpenResults{}, err
	}
	if err := ctx.Err(); err != nil {
		s.releaseStoreResults(storeResults)
		return OpenResults{}, err
	}
	validatedResults, err := NewOpenResults(results)
	if err != nil {
		s.releaseStoreResults(storeResults)
		return OpenResults{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.releaseStoreResults(storeResults)
		return OpenResults{}, ErrServiceClosed
	}
	for leaseID := range leases {
		_, active := s.leases[leaseID]
		_, released := s.released[leaseID]
		if active || released {
			s.mu.Unlock()
			s.releaseStoreResults(storeResults)
			return OpenResults{}, fmt.Errorf("%w: session lease identity reused", ErrRevisionStoreContract)
		}
	}
	for leaseID, descriptor := range leases {
		s.leases[leaseID] = descriptor
	}
	s.mu.Unlock()
	return validatedResults, nil
}

func (s *SenderService) prepareOpenResults(
	items []OpenItem,
	storeResults []content.OpenRevisionResult,
) ([]OpenResult, map[content.LeaseID]content.FileRevisionDescriptor, error) {
	results := make([]OpenResult, len(storeResults))
	seen := make(map[content.LeaseID]struct{}, len(storeResults))
	leases := make(map[content.LeaseID]content.FileRevisionDescriptor, len(storeResults))
	for index, result := range storeResults {
		prepared, successful, err := s.prepareOpenResult(items[index].FileID, result, seen)
		if err != nil {
			return nil, nil, err
		}
		results[index] = prepared
		if successful {
			leases[result.Lease.ID()] = result.Lease.Descriptor()
		}
	}
	return results, leases, nil
}

func (s *SenderService) prepareOpenResult(
	expected catalog.FileID,
	result content.OpenRevisionResult,
	seen map[content.LeaseID]struct{},
) (OpenResult, bool, error) {
	if result.FileID != expected {
		return OpenResult{}, false, fmt.Errorf("%w: changed open result order or identity", ErrRevisionStoreContract)
	}
	if result.Err != nil {
		if !result.Lease.ID().IsZero() {
			return OpenResult{}, false, fmt.Errorf("%w: open result contains both a lease and an error", ErrRevisionStoreContract)
		}
		failure := classifyRevisionError(result.Err)
		prepared, err := FailedOpen(result.FileID, failure)
		return prepared, false, err
	}
	if err := validateOpenedLease(result); err != nil {
		return OpenResult{}, false, err
	}
	if _, duplicate := seen[result.Lease.ID()]; duplicate {
		return OpenResult{}, false, fmt.Errorf("%w: lease identity reused within one batch", ErrRevisionStoreContract)
	}
	seen[result.Lease.ID()] = struct{}{}
	object, err := s.sealer.SealRevision(result.Lease.Descriptor())
	if err != nil {
		_ = s.store.ReleaseLease(result.Lease.ID())
		failure := classifyRevisionError(err)
		prepared, prepareErr := FailedOpen(result.FileID, failure)
		return prepared, false, prepareErr
	}
	prepared, err := SuccessfulOpen(result.FileID, result.Lease, object)
	return prepared, true, err
}

func validateOpenedLease(result content.OpenRevisionResult) error {
	if result.Lease.ID().IsZero() || result.Lease.Descriptor().FileID() != result.FileID ||
		result.Lease.TTL() <= 0 || result.Lease.RenewAfter() < 0 || result.Lease.RenewAfter() > result.Lease.TTL() {
		return fmt.Errorf("%w: invalid successful lease", ErrRevisionStoreContract)
	}
	return nil
}

func (s *SenderService) Renew(leaseID content.LeaseID) (content.RevisionLease, error) {
	descriptor, err := s.ownedDescriptor(leaseID)
	if err != nil {
		return content.RevisionLease{}, err
	}
	lease, err := s.store.RenewLease(leaseID)
	if err != nil {
		switch {
		case errors.Is(err, content.ErrRevisionDrift), errors.Is(err, content.ErrSourceDrift):
			s.invalidateOwnedRevision(descriptor)
		case errors.Is(err, content.ErrLeaseExpired), errors.Is(err, content.ErrLeaseLifetime), errors.Is(err, content.ErrInvalidLease):
			s.retireLease(leaseID)
		}
		return content.RevisionLease{}, err
	}
	if err := validateRenewedLease(leaseID, descriptor, lease); err != nil {
		s.retireLease(leaseID)
		return content.RevisionLease{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = s.store.ReleaseLease(leaseID)
		return content.RevisionLease{}, ErrServiceClosed
	}
	if current, exists := s.leases[leaseID]; !exists || current != descriptor {
		s.mu.Unlock()
		_ = s.store.ReleaseLease(leaseID)
		return content.RevisionLease{}, ErrLeaseNotOwned
	}
	s.mu.Unlock()
	return lease, nil
}

func validateRenewedLease(
	requested content.LeaseID,
	descriptor content.FileRevisionDescriptor,
	lease content.RevisionLease,
) error {
	if lease.ID() != requested || lease.Descriptor() != descriptor || lease.TTL() <= 0 ||
		lease.RenewAfter() < 0 || lease.RenewAfter() > lease.TTL() {
		return ErrRevisionStoreContract
	}
	return nil
}

func (s *SenderService) Release(leaseID content.LeaseID) error {
	if leaseID.IsZero() {
		return ErrInvalidLeaseRequest
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrServiceClosed
	}
	if _, known := s.released[leaseID]; known {
		s.mu.Unlock()
		return nil
	}
	if _, owned := s.leases[leaseID]; !owned {
		s.mu.Unlock()
		// RELEASE is intentionally non-oracular: an unknown ID succeeds without
		// touching the share-scoped store. This keeps replay idempotent after the
		// bounded identity tombstone rotates and cannot release another session's
		// lease.
		return nil
	}
	delete(s.leases, leaseID)
	s.rememberReleasedLocked(leaseID)
	s.mu.Unlock()
	return s.store.ReleaseLease(leaseID)
}

func (s *SenderService) ServeBlocks(
	ctx context.Context,
	operationID protocolsession.OperationID,
	request BlockRequest,
	emit func(context.Context, protocolsession.Message) error,
) (uint32, error) {
	if operationID.IsZero() || emit == nil {
		return 0, ErrOperationIdentity
	}
	validated, err := NewBlockRequest(request.leaseID, request.indices)
	if err != nil {
		return 0, err
	}
	descriptor, err := s.ownedDescriptor(validated.leaseID)
	if err != nil {
		return 0, errors.Join(errRevisionOperationScope, err)
	}
	workContext, cancelWork := context.WithCancelCause(ctx)
	unwatch, err := s.cache.watchRevision(descriptor, cancelWork)
	if err != nil {
		cancelWork(nil)
		if errors.Is(err, content.ErrRevisionDrift) {
			s.invalidateOwnedRevision(descriptor)
			return 0, errors.Join(errRevisionOperationScope, err)
		}
		return 0, err
	}
	defer func() {
		unwatch()
		cancelWork(nil)
	}()
	for delivered, index := range validated.indices {
		if err := context.Cause(workContext); err != nil {
			return uint32(delivered), s.normalizeWorkCause(descriptor, err)
		}
		object, err := s.blockObject(workContext, validated.leaseID, descriptor, index)
		if err != nil {
			if cause := context.Cause(workContext); cause != nil {
				return uint32(delivered), s.normalizeWorkCause(descriptor, cause)
			}
			return uint32(delivered), err
		}
		if err := emitBlockObject(workContext, operationID, object, emit); err != nil {
			if cause := context.Cause(workContext); cause != nil {
				return uint32(delivered), s.normalizeWorkCause(descriptor, cause)
			}
			return uint32(delivered), err
		}
	}
	return uint32(len(validated.indices)), nil
}

func (s *SenderService) normalizeWorkCause(descriptor content.FileRevisionDescriptor, cause error) error {
	if errors.Is(cause, content.ErrRevisionDrift) {
		s.invalidateOwnedRevision(descriptor)
		return errors.Join(errRevisionOperationScope, cause)
	}
	return cause
}

func (s *SenderService) blockObject(
	ctx context.Context,
	leaseID content.LeaseID,
	descriptor content.FileRevisionDescriptor,
	index uint64,
) ([]byte, error) {
	if err := s.store.ValidateLease(leaseID, descriptor); err != nil {
		if errors.Is(err, content.ErrRevisionDrift) || errors.Is(err, content.ErrSourceDrift) {
			s.invalidateOwnedRevision(descriptor)
		} else if errors.Is(err, content.ErrLeaseExpired) || errors.Is(err, content.ErrInvalidLease) {
			s.retireLease(leaseID)
		}
		return nil, errors.Join(errRevisionOperationScope, err)
	}
	ref, err := content.NewBlockRef(descriptor.FileID(), descriptor.FileRevision(), index, descriptor.Geometry())
	if err != nil {
		return nil, err
	}
	cacheKey, err := NewBlockCacheKey(descriptor, index)
	if err != nil {
		return nil, err
	}
	object, err := s.cache.Get(ctx, cacheKey, func(loadContext context.Context) ([]byte, error) {
		return s.readAndSealBlock(loadContext, leaseID, descriptor, index, ref)
	})
	if errors.Is(err, content.ErrRevisionDrift) || errors.Is(err, content.ErrSourceDrift) {
		s.invalidateOwnedRevision(descriptor)
		return object, errors.Join(errRevisionOperationScope, err)
	}
	return object, err
}

func (s *SenderService) readAndSealBlock(
	ctx context.Context,
	leaseID content.LeaseID,
	descriptor content.FileRevisionDescriptor,
	index uint64,
	ref content.BlockRef,
) ([]byte, error) {
	plaintext, err := s.store.ReadBlock(ctx, leaseID, ref)
	if err != nil {
		return nil, errors.Join(errRevisionOperationScope, err)
	}
	record, err := records.NewBlockRecord(descriptor, index, plaintext)
	if err != nil {
		return nil, errors.Join(errRevisionOperationScope, err)
	}
	sealed, err := s.sealer.SealBlock(record)
	if err != nil {
		return nil, errors.Join(errRevisionOperationScope, err)
	}
	return sealed.Object, nil
}

func emitBlockObject(
	ctx context.Context,
	operationID protocolsession.OperationID,
	object []byte,
	emit func(context.Context, protocolsession.Message) error,
) error {
	fragments, err := FragmentRecord(operationID, object)
	if err != nil {
		return err
	}
	for _, fragment := range fragments {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if err := emit(ctx, fragment); err != nil {
			return err
		}
	}
	return nil
}

func (s *SenderService) releaseStoreResults(results []content.OpenRevisionResult) {
	for _, result := range results {
		if !result.Lease.ID().IsZero() {
			_ = s.store.ReleaseLease(result.Lease.ID())
		}
	}
}

func (s *SenderService) releaseOpenResults(results OpenResults) error {
	var result error
	for _, opened := range results.Items() {
		if opened.Failure == nil {
			result = errors.Join(result, s.Release(opened.Lease.ID()))
		}
	}
	return result
}

func (s *SenderService) retireLease(leaseID content.LeaseID) {
	s.mu.Lock()
	if _, exists := s.leases[leaseID]; exists {
		delete(s.leases, leaseID)
		if !s.closed {
			s.rememberReleasedLocked(leaseID)
		}
	}
	s.mu.Unlock()
	_ = s.store.ReleaseLease(leaseID)
}

func (s *SenderService) invalidateOwnedRevision(descriptor content.FileRevisionDescriptor) {
	s.cache.InvalidateRevision(descriptor.FileID(), descriptor.FileRevision())
	s.mu.Lock()
	leases := make([]content.LeaseID, 0)
	for leaseID, owned := range s.leases {
		if owned.FileID() == descriptor.FileID() && owned.FileRevision() == descriptor.FileRevision() {
			delete(s.leases, leaseID)
			s.rememberReleasedLocked(leaseID)
			leases = append(leases, leaseID)
		}
	}
	s.mu.Unlock()
	for _, leaseID := range leases {
		_ = s.store.ReleaseLease(leaseID)
	}
}

func (s *SenderService) ownedDescriptor(leaseID content.LeaseID) (content.FileRevisionDescriptor, error) {
	if leaseID.IsZero() {
		return content.FileRevisionDescriptor{}, ErrInvalidLeaseRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return content.FileRevisionDescriptor{}, ErrServiceClosed
	}
	descriptor, ok := s.leases[leaseID]
	if !ok {
		return content.FileRevisionDescriptor{}, ErrLeaseNotOwned
	}
	return descriptor, nil
}

func (s *SenderService) checkOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrServiceClosed
	}
	return nil
}

func (s *SenderService) rememberReleasedLocked(leaseID content.LeaseID) {
	if len(s.releasedOrder) < ReleasedLeaseTombstoneLimit {
		s.releasedOrder = append(s.releasedOrder, leaseID)
	} else {
		delete(s.released, s.releasedOrder[s.releasedCursor])
		s.releasedOrder[s.releasedCursor] = leaseID
		s.releasedCursor = (s.releasedCursor + 1) % ReleasedLeaseTombstoneLimit
	}
	s.released[leaseID] = struct{}{}
}

func (s *SenderService) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	leases := make([]content.LeaseID, 0, len(s.leases))
	for leaseID := range s.leases {
		leases = append(leases, leaseID)
	}
	clear(s.leases)
	s.mu.Unlock()
	var result error
	for _, leaseID := range leases {
		result = errors.Join(result, s.store.ReleaseLease(leaseID))
	}
	return result
}

func classifyRevisionError(err error) RevisionFailure {
	code := RevisionCodeUnreadable
	switch {
	case errors.Is(err, content.ErrRevisionStale):
		code = RevisionCodeStale
	case errors.Is(err, content.ErrRevisionNotFound):
		code = RevisionCodeNotFound
	case errors.Is(err, content.ErrUnsupportedStability):
		code = RevisionCodeUnsupportedStability
	case errors.Is(err, content.ErrQuotaExceeded), errors.Is(err, records.ErrSealLimit):
		code = RevisionCodeQuota
	case errors.Is(err, content.ErrLeaseExpired), errors.Is(err, content.ErrLeaseLifetime):
		code = RevisionCodeLeaseExpired
	case errors.Is(err, content.ErrRevisionDrift), errors.Is(err, content.ErrSourceDrift):
		code = RevisionCodeDrift
	case errors.Is(err, content.ErrInvalidLease), errors.Is(err, content.ErrRenewTooEarly), errors.Is(err, ErrLeaseNotOwned):
		code = RevisionCodeInvalidLease
	}
	failure, _ := NewRevisionFailure(code, false, 0)
	return failure
}

func classifyBlockError(err error) uint16 {
	switch {
	case errors.Is(err, content.ErrInvalidBlockRef), errors.Is(err, ErrInvalidBlockRequest):
		return BlockCodeInvalidRef
	case errors.Is(err, content.ErrBlockOutOfRange):
		return BlockCodeOutOfRange
	case errors.Is(err, records.ErrObjectAuth), errors.Is(err, records.ErrObjectSignature), errors.Is(err, ErrRecordDigest):
		return BlockCodeObjectAuth
	case errors.Is(err, ErrFragmentConflict):
		return BlockCodeFragmentConflict
	case errors.Is(err, ErrFragmentTimeout), errors.Is(err, context.DeadlineExceeded):
		return BlockCodeTimeout
	case errors.Is(err, context.Canceled), errors.Is(err, ErrFragmentCancelled):
		return BlockCodeCancelled
	default:
		return BlockCodeInvalidRef
	}
}

func wrapServiceError(action string, err error) error {
	return fmt.Errorf("content sender %s: %w", action, err)
}
