package catalog

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
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

// Retryable reports whether a later explicit scan attempt may succeed. Budget
// exhaustion reflects runtime capacity (concurrent scans or sibling shares),
// not an immutable property of the directory, so it must never brick a share.
func (r DirectoryFailureRecord) Retryable() bool {
	return r.kind == FailureKindTransientIO || r.kind == FailureKindBudget
}

func (r DirectoryFailureRecord) RetryAfter() time.Duration { return r.retryAfter }

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

const (
	// MaxFailurePayloadBytes bounds a serialized directory failure record. Wire
	// directory failures contain only fixed scalar identities, codes, and retry
	// delays (~70 bytes), so reserving a full 60 KiB page payload would block
	// failure reporting whenever remaining memory is tight.
	MaxFailurePayloadBytes          = 1 << 10
	failureEncodingReservationBytes = memoryFailureOverhead + MaxFailurePayloadBytes
)

// FailurePayload carries the bytes committed with a directory scan failure.
type FailurePayload struct {
	encoded    []byte
	commitment [sha256.Size]byte
}

func NewFailurePayload(encoded []byte) (FailurePayload, error) {
	if len(encoded) == 0 || len(encoded) > MaxFailurePayloadBytes {
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

type preparedFailure struct {
	record  DirectoryFailureRecord
	payload FailurePayload
	charge  ResourceUsage
}

func (s *CatalogStore) prepareAttemptFailure(
	attempt *scanAttempt,
	cause error,
) (preparedFailure, error) {
	kind := classifyFailureKind(cause)
	record, err := newDirectoryFailureRecord(
		s.shareInstance, attempt.directory, attempt.id, attempt.generation, attempt.previous,
		kind, failureRetryAfter(kind, cause),
	)
	if err != nil {
		return preparedFailure{}, err
	}
	if err := attempt.resources.Consume(ResourceUsage{MemoryBytes: failureEncodingReservationBytes}); err != nil {
		return preparedFailure{}, err
	}
	payload, encodeErr := s.failureEncoder.EncodeFailure(record)
	if encodeErr != nil {
		return preparedFailure{}, errors.Join(
			encodeErr,
			attempt.resources.Release(ResourceUsage{MemoryBytes: failureEncodingReservationBytes}),
		)
	}
	if payload.IsZero() || len(payload.encoded) > MaxFailurePayloadBytes {
		return preparedFailure{}, errors.Join(
			errors.New("catalog failure encoder returned an invalid payload"),
			attempt.resources.Release(ResourceUsage{MemoryBytes: failureEncodingReservationBytes}),
		)
	}
	charge := ResourceUsage{MemoryBytes: memoryFailureOverhead + uint64(len(payload.encoded))}
	if failureEncodingReservationBytes > charge.MemoryBytes {
		if err := attempt.resources.Release(ResourceUsage{MemoryBytes: failureEncodingReservationBytes - charge.MemoryBytes}); err != nil {
			return preparedFailure{}, err
		}
	}
	return preparedFailure{record: record, payload: payload, charge: charge}, nil
}

func (s *CatalogStore) commitPreparedFailureLocked(
	attempt *scanAttempt,
	prepared preparedFailure,
	cause error,
	session *BudgetAccount,
) (*DirectoryFailure, error) {
	if s.closed {
		return nil, ErrCatalogClosed
	}
	if s.attempts[prepared.record.directory] != attempt {
		return nil, context.Canceled
	}
	attempts := s.failures[prepared.record.directory]
	if len(attempts) >= maxFailureAttemptsPerDirectory {
		return nil, ErrBudgetExceeded
	}
	retained, err := attempt.resources.retain(prepared.charge, session)
	if err != nil {
		return nil, err
	}
	if attempts == nil {
		attempts = make(map[ScanAttemptID]committedFailure)
		s.failures[prepared.record.directory] = attempts
	}
	attempts[prepared.record.attempt] = committedFailure{record: prepared.record, payload: prepared.payload}
	s.held = append(s.held, retained)
	return directoryFailureFromRecord(prepared.record, cause), nil
}

func classifyFailureKind(cause error) FailureKind {
	var scanError *ScanError
	switch {
	case errors.Is(cause, ErrDirectoryStale):
		return FailureKindStale
	case errors.Is(cause, fs.ErrPermission):
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

func failureRetryAfter(kind FailureKind, cause error) time.Duration {
	if kind != FailureKindTransientIO && kind != FailureKindBudget {
		return 0
	}
	var scanError *ScanError
	if errors.As(cause, &scanError) && scanError.transient {
		return scanError.cooldown
	}
	return DefaultScanRetryCooldown
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
	if err := ctx.Err(); err != nil {
		return FailurePayload{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return FailurePayload{}, false, ErrCatalogClosed
	}
	failure, found := s.failures[directory][attempt]
	if !found {
		return FailurePayload{}, false, nil
	}
	if _, known := s.usedAttempts[scanAttemptIdentity{directory: directory, attempt: attempt}]; !known {
		return FailurePayload{}, false, errors.New("catalog failure payload lacks attempt authority")
	}
	return failure.payload, true, nil
}

// completeFailedAttempt checks attempt ownership, commits any prepared failure
// payload, and finalizes the attempt under a single s.mu lock so a cancelled
// scan cannot leak failure records or overwrite a successor attempt.
func (s *CatalogStore) completeFailedAttempt(
	attempt *scanAttempt,
	cause error,
	prepared *preparedFailure,
	prepareErr error,
	session *BudgetAccount,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if attempt.completed {
		return
	}
	attempt.completed = true
	defer finishAttemptLocked(attempt)

	if s.closed || s.attempts[attempt.directory] != attempt {
		attempt.err = cause
		return
	}
	var (
		failure    *DirectoryFailure
		persistErr = prepareErr
	)
	if persistErr == nil && prepared != nil {
		failure, persistErr = s.commitPreparedFailureLocked(attempt, *prepared, cause, session)
	}
	if persistErr != nil {
		attempt.err = errors.Join(cause, fmt.Errorf("persist catalog directory failure: %w", persistErr))
		delete(s.attempts, attempt.directory)
		return
	}
	if failure == nil {
		attempt.err = cause
		delete(s.attempts, attempt.directory)
		return
	}
	attempt.failure = failure
	attempt.err = failure
	attempt.retryable = failure.Transient
	if attempt.retryable {
		attempt.retryAt = s.clock.Now().Add(failure.RetryAfter)
	}
}
