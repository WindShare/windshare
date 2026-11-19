package catalog

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"time"
)

type FailureKind uint8

const (
	FailureKindUnknown FailureKind = iota
	FailureKindStale
	FailureKindPermission
	FailureKindCollision
	FailureKindTooWide
	FailureKindBudget
	FailureKindPermanentIO
	FailureKindTransientIO

	maxFailureAttemptsPerDirectory = 16_384
)

type DirectoryFailureRecord struct {
	share           ShareInstance
	directory       DirectoryID
	attempt         ScanAttemptID
	generation      DirectoryGeneration
	previousAttempt ScanAttemptID
	kind            FailureKind
	retryAfter      time.Duration
}

func newDirectoryFailureRecord(
	share ShareInstance,
	directory DirectoryID,
	attempt ScanAttemptID,
	generation DirectoryGeneration,
	previousAttempt ScanAttemptID,
	kind FailureKind,
	retryAfter time.Duration,
) (DirectoryFailureRecord, error) {
	record := DirectoryFailureRecord{
		share: share, directory: directory, attempt: attempt, generation: generation,
		previousAttempt: previousAttempt, kind: kind, retryAfter: retryAfter,
	}
	if err := record.valid(); err != nil {
		return DirectoryFailureRecord{}, err
	}
	return record, nil
}

func (r DirectoryFailureRecord) ShareInstance() ShareInstance     { return r.share }
func (r DirectoryFailureRecord) DirectoryID() DirectoryID         { return r.directory }
func (r DirectoryFailureRecord) AttemptID() ScanAttemptID         { return r.attempt }
func (r DirectoryFailureRecord) Generation() DirectoryGeneration  { return r.generation }
func (r DirectoryFailureRecord) PreviousAttemptID() ScanAttemptID { return r.previousAttempt }
func (r DirectoryFailureRecord) Kind() FailureKind                { return r.kind }
func (r DirectoryFailureRecord) Retryable() bool                  { return r.kind == FailureKindTransientIO }
func (r DirectoryFailureRecord) RetryAfter() time.Duration        { return r.retryAfter }

func (r DirectoryFailureRecord) valid() error {
	if r.share.IsZero() || r.directory.IsZero() || r.attempt.IsZero() || r.generation.IsZero() ||
		r.kind <= FailureKindUnknown || r.kind > FailureKindTransientIO {
		return errors.New("catalog failure record has invalid identity or kind")
	}
	if r.Retryable() {
		if r.retryAfter < MinScanRetryCooldown || r.retryAfter > MaxScanRetryCooldown ||
			r.retryAfter%time.Millisecond != 0 {
			return errors.New("catalog transient failure has an invalid retry delay")
		}
	} else if r.retryAfter != 0 {
		return errors.New("catalog permanent failure carries a retry delay")
	}
	if r.previousAttempt == r.attempt {
		return errors.New("catalog failure attempt points to itself")
	}
	return nil
}

// FailurePayload carries the bytes committed with a directory scan failure.
// Its digest lets the backend check payload integrity on replay.
type FailurePayload struct {
	encoded    []byte
	commitment [sha256.Size]byte
}

func NewFailurePayload(encoded []byte) (FailurePayload, error) {
	if len(encoded) == 0 || len(encoded) > MaxCatalogPayloadBytes {
		return FailurePayload{}, fmt.Errorf("catalog failure payload has invalid length %d", len(encoded))
	}
	return FailurePayload{encoded: append([]byte(nil), encoded...), commitment: sha256.Sum256(encoded)}, nil
}

func (o FailurePayload) Bytes() []byte { return append([]byte(nil), o.encoded...) }
func (o FailurePayload) IsZero() bool  { return len(o.encoded) == 0 }

type FailureEncoder interface {
	EncodeFailure(DirectoryFailureRecord) (FailurePayload, error)
}

type FailureEncoderFunc func(DirectoryFailureRecord) (FailurePayload, error)

func (function FailureEncoderFunc) EncodeFailure(record DirectoryFailureRecord) (FailurePayload, error) {
	return function(record)
}

type memoryFailure struct {
	record  DirectoryFailureRecord
	payload []byte
	usage   ResourceUsage
}

func (b *MemoryCatalogBackend) CommitFailure(
	ctx context.Context,
	record DirectoryFailureRecord,
	payload FailurePayload,
	meter ResourceMeter,
	prepare func(ResourceUsage) error,
) (FailurePreparation, error) {
	if err := ctx.Err(); err != nil {
		return FailurePreparation{}, err
	}
	if err := record.valid(); err != nil || payload.IsZero() || meter == nil || prepare == nil {
		return FailurePreparation{}, errors.Join(errors.New("catalog failure commit is invalid"), err)
	}
	charge := ResourceUsage{MemoryBytes: memoryObjectOverhead*2 + uint64(len(payload.encoded))}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return FailurePreparation{}, ErrCatalogClosed
	}
	attempts := b.failures[record.directory]
	if existing, found := attempts[record.attempt]; found {
		candidate := sha256.Sum256(existing.payload)
		if existing.record != record || candidate != payload.commitment {
			return FailurePreparation{}, ErrGenerationConflict
		}
		return FailurePreparation{Record: record, Existing: true}, nil
	}
	if len(attempts) >= maxFailureAttemptsPerDirectory {
		return FailurePreparation{}, ErrBudgetExceeded
	}
	records := make([]DirectoryFailureRecord, 0, len(attempts))
	for _, failure := range attempts {
		records = append(records, failure.record)
	}
	tail, err := validateFailureChain(records)
	if err != nil || tail != record.previousAttempt {
		return FailurePreparation{}, errors.Join(ErrGenerationConflict, err)
	}
	if err := meter.Consume(charge); err != nil {
		return FailurePreparation{}, err
	}
	if err := prepare(charge); err != nil {
		return FailurePreparation{}, err
	}
	if attempts == nil {
		attempts = make(map[ScanAttemptID]memoryFailure)
		b.failures[record.directory] = attempts
	}
	attempts[record.attempt] = memoryFailure{record: record, payload: payload.Bytes(), usage: charge}
	return FailurePreparation{Record: record, Usage: charge}, nil
}

func (b *MemoryCatalogBackend) LoadFailurePayload(
	ctx context.Context,
	directory DirectoryID,
	attempt ScanAttemptID,
) (FailurePayload, bool, error) {
	if err := ctx.Err(); err != nil {
		return FailurePayload{}, false, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return FailurePayload{}, false, ErrCatalogClosed
	}
	failure, found := b.failures[directory][attempt]
	if !found {
		return FailurePayload{}, false, nil
	}
	payload, err := NewFailurePayload(failure.payload)
	return payload, err == nil, err
}

func (b *MemoryCatalogBackend) ReplayFailures(ctx context.Context, yield func(DirectoryFailureRecord, bool) error) error {
	if yield == nil {
		return errors.New("catalog failure replay requires a consumer")
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return ErrCatalogClosed
	}
	for _, attempts := range b.failures {
		if len(attempts) > maxFailureAttemptsPerDirectory {
			return ErrCorruptCatalogStorage
		}
		records := make([]DirectoryFailureRecord, 0, len(attempts))
		for _, failure := range attempts {
			records = append(records, failure.record)
		}
		tail, err := validateFailureChain(records)
		if err != nil {
			return err
		}
		for _, failure := range attempts {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := yield(failure.record, failure.record.attempt == tail); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateFailureChain(records []DirectoryFailureRecord) (ScanAttemptID, error) {
	if len(records) == 0 {
		return ScanAttemptID{}, nil
	}
	byAttempt := make(map[ScanAttemptID]DirectoryFailureRecord, len(records))
	successors := make(map[ScanAttemptID]ScanAttemptID, len(records))
	roots := 0
	for _, record := range records {
		if err := record.valid(); err != nil {
			return ScanAttemptID{}, ErrCorruptCatalogStorage
		}
		if _, duplicate := byAttempt[record.attempt]; duplicate {
			return ScanAttemptID{}, ErrCorruptCatalogStorage
		}
		byAttempt[record.attempt] = record
		if record.previousAttempt.IsZero() {
			roots++
			continue
		}
		if _, fork := successors[record.previousAttempt]; fork {
			return ScanAttemptID{}, ErrCorruptCatalogStorage
		}
		successors[record.previousAttempt] = record.attempt
	}
	if roots != 1 {
		return ScanAttemptID{}, ErrCorruptCatalogStorage
	}
	var tail ScanAttemptID
	tails := 0
	for attempt := range byAttempt {
		if _, hasSuccessor := successors[attempt]; !hasSuccessor {
			tail = attempt
			tails++
		}
	}
	if tails != 1 {
		return ScanAttemptID{}, ErrCorruptCatalogStorage
	}
	visited := 0
	for current := tail; !current.IsZero(); {
		record, found := byAttempt[current]
		if !found {
			return ScanAttemptID{}, ErrCorruptCatalogStorage
		}
		visited++
		if visited > len(records) {
			return ScanAttemptID{}, ErrCorruptCatalogStorage
		}
		current = record.previousAttempt
	}
	if visited != len(records) {
		return ScanAttemptID{}, ErrCorruptCatalogStorage
	}
	return tail, nil
}

func (s *CatalogStore) restoreFailureAuthority(ctx context.Context) error {
	return s.backend.ReplayFailures(ctx, func(record DirectoryFailureRecord, active bool) error {
		if record.share != s.shareInstance {
			return ErrCorruptCatalogStorage
		}
		identity := scanAttemptIdentity{directory: record.directory, attempt: record.attempt}
		if _, duplicate := s.usedAttempts[identity]; duplicate {
			return ErrCorruptCatalogStorage
		}
		if _, duplicate := s.usedGenerations[record.generation]; duplicate {
			return ErrCorruptCatalogStorage
		}
		s.usedAttempts[identity] = struct{}{}
		s.usedGenerations[record.generation] = struct{}{}
		if !active {
			return nil
		}
		if s.attempts[record.directory] != nil {
			return ErrCorruptCatalogStorage
		}
		failure := directoryFailureFromRecord(record, errors.New("recovered catalog directory failure"))
		done := make(chan struct{})
		close(done)
		attempt := &scanAttempt{
			id: record.attempt, generation: record.generation, directory: record.directory,
			previous: record.previousAttempt, done: done, completed: true, err: failure,
			retryable: record.Retryable(), failure: failure,
		}
		if record.Retryable() {
			attempt.retryAt = s.clock.Now().Add(record.retryAfter)
		}
		s.attempts[record.directory] = attempt
		return nil
	})
}

func (s *CatalogStore) persistAttemptFailure(
	attempt *scanAttempt,
	cause error,
	session *BudgetAccount,
) (*DirectoryFailure, error) {
	record, err := newDirectoryFailureRecord(
		s.shareInstance, attempt.directory, attempt.id, attempt.generation, attempt.previous,
		classifyFailureKind(cause), failureRetryAfter(cause),
	)
	if err != nil {
		return nil, err
	}
	if err := attempt.resources.Consume(ResourceUsage{MemoryBytes: MaxCatalogPayloadBytes}); err != nil {
		return nil, err
	}
	payload, encodeErr := s.failureEncoder.EncodeFailure(record)
	releaseErr := attempt.resources.Release(ResourceUsage{MemoryBytes: MaxCatalogPayloadBytes})
	if err := errors.Join(encodeErr, releaseErr); err != nil {
		return nil, err
	}
	var retained *BudgetReservation
	prepared, err := s.backend.CommitFailure(
		attempt.ctx, record, payload, attempt.resources,
		func(usage ResourceUsage) error {
			var retainErr error
			retained, retainErr = attempt.resources.retain(usage, session)
			return retainErr
		},
	)
	if err != nil {
		retained.Release()
		return nil, err
	}
	if !prepared.Existing {
		if retained == nil || !retained.active() {
			return nil, errors.New("catalog failure publication lost its retained budget")
		}
		s.mu.Lock()
		s.held = append(s.held, retained)
		s.mu.Unlock()
	}
	return directoryFailureFromRecord(record, cause), nil
}

func classifyFailureKind(cause error) FailureKind {
	var scanError *ScanError
	switch {
	case errors.Is(cause, ErrDirectoryStale):
		return FailureKindStale
	case errors.Is(cause, os.ErrPermission):
		return FailureKindPermission
	case errors.Is(cause, ErrSiblingCollision):
		return FailureKindCollision
	case errors.Is(cause, ErrPageLimit):
		return FailureKindTooWide
	case errors.Is(cause, ErrBudgetExceeded):
		return FailureKindBudget
	case errors.As(cause, &scanError) && scanError.transient:
		return FailureKindTransientIO
	default:
		return FailureKindPermanentIO
	}
}

func failureRetryAfter(cause error) time.Duration {
	var scanError *ScanError
	if errors.As(cause, &scanError) && scanError.transient {
		return scanError.cooldown
	}
	return 0
}

func directoryFailureFromRecord(record DirectoryFailureRecord, cause error) *DirectoryFailure {
	return &DirectoryFailure{
		DirectoryID: record.directory, AttemptID: record.attempt, Kind: record.kind,
		Transient: record.Retryable(), RetryAfter: record.retryAfter, cause: cause, record: record,
	}
}

func (s *CatalogStore) FailurePayload(
	ctx context.Context,
	directory DirectoryID,
	attempt ScanAttemptID,
) (FailurePayload, bool, error) {
	if s.isClosed() {
		return FailurePayload{}, false, ErrCatalogClosed
	}
	payload, found, err := s.backend.LoadFailurePayload(ctx, directory, attempt)
	if err != nil || !found {
		return FailurePayload{}, found, err
	}
	s.mu.Lock()
	_, known := s.usedAttempts[scanAttemptIdentity{directory: directory, attempt: attempt}]
	s.mu.Unlock()
	if !known {
		return FailurePayload{}, false, ErrCorruptCatalogStorage
	}
	return payload, true, nil
}

func (s *CatalogStore) completeFailedAttemptLocked(attempt *scanAttempt, resultErr error) {
	switch {
	case attempt.failure != nil:
		attempt.err = attempt.failure
		attempt.retryable = attempt.failure.Transient
		if attempt.retryable {
			attempt.retryAt = s.clock.Now().Add(attempt.failure.RetryAfter)
		}
		if !s.closed {
			s.attempts[attempt.directory] = attempt
		}
	case errors.Is(resultErr, context.Canceled) && s.attempts[attempt.directory] != attempt:
		attempt.err = resultErr
	default:
		attempt.err = resultErr
		if s.attempts[attempt.directory] == attempt {
			delete(s.attempts, attempt.directory)
		}
	}
}
