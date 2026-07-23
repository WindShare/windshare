package catalog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ScanProgressEntryInterval matches one maximum catalog page. The first child
// is reported immediately; later milestones are coalesced so a slow receiver
// cannot turn waiting feedback into scan-wide backpressure.
const ScanProgressEntryInterval = uint64(MaxCatalogPageEntries)

type ScanWork interface {
	Consume(uint64) error
}

type ScanChildSink interface {
	Add(context.Context, ScannedChild) error
}

type ScanRequest struct {
	AttemptID  ScanAttemptID
	Generation DirectoryGeneration
	Directory  NodeRecord
	Work       ScanWork
	Children   ScanChildSink
}

type ScanResult struct {
	OmittedCount uint64
}

type DirectoryScanner interface {
	// ScanDirectory must validate the directory identity before enumeration and
	// again before returning nil. Children remain provisional until this method
	// returns, so stale, cancellation, and reparse failures cannot publish pages.
	ScanDirectory(context.Context, ScanRequest) (ScanResult, error)
}

type DirectoryScannerFunc func(context.Context, ScanRequest) (ScanResult, error)

func (f DirectoryScannerFunc) ScanDirectory(ctx context.Context, request ScanRequest) (ScanResult, error) {
	return f(ctx, request)
}

type ScanProgress struct {
	AttemptID         ScanAttemptID
	DiscoveredEntries uint64
}

type ScanProgressObserver interface {
	ObserveScanProgress(context.Context, ScanProgress) error
}

type ScanProgressObserverFunc func(context.Context, ScanProgress) error

func (observe ScanProgressObserverFunc) ObserveScanProgress(
	ctx context.Context,
	progress ScanProgress,
) error {
	if observe == nil {
		return errors.New("catalog scan progress observer is nil")
	}
	return observe(ctx, progress)
}

type ScanOptions struct {
	Retry    bool
	Progress ScanProgressObserver
}

type ScanError struct {
	cause     error
	transient bool
	cooldown  time.Duration
}

const (
	MinScanRetryCooldown     = 250 * time.Millisecond
	DefaultScanRetryCooldown = time.Second
	MaxScanRetryCooldown     = 30 * time.Second
)

func NewTransientScanError(cause error, cooldown time.Duration) error {
	if cause == nil {
		cause = errors.New("transient catalog scan failure")
	}
	switch {
	case cooldown == 0:
		cooldown = DefaultScanRetryCooldown
	case cooldown < MinScanRetryCooldown:
		cooldown = MinScanRetryCooldown
	case cooldown > MaxScanRetryCooldown:
		cooldown = MaxScanRetryCooldown
	}
	cooldown = ((cooldown + time.Millisecond - 1) / time.Millisecond) * time.Millisecond
	return &ScanError{cause: cause, transient: true, cooldown: cooldown}
}

func NewPermanentScanError(cause error) error {
	if cause == nil {
		cause = errors.New("permanent catalog scan failure")
	}
	return &ScanError{cause: cause}
}

func (e *ScanError) Error() string { return e.cause.Error() }
func (e *ScanError) Unwrap() error { return e.cause }

var ErrDirectoryStale = errors.New("catalog directory identity became stale")

var ErrScanSinkClosed = errors.New("catalog scan child sink is closed")

type DirectoryFailure struct {
	DirectoryID DirectoryID
	AttemptID   ScanAttemptID
	Kind        FailureKind
	Transient   bool
	RetryAfter  time.Duration
	cause       error
	record      DirectoryFailureRecord
}

func (e *DirectoryFailure) Error() string {
	if e.Transient {
		return fmt.Sprintf("catalog scan %x for directory %x failed transiently: %v", e.AttemptID, e.DirectoryID, e.cause)
	}
	return fmt.Sprintf("catalog scan %x for directory %x failed permanently: %v", e.AttemptID, e.DirectoryID, e.cause)
}

func (e *DirectoryFailure) Unwrap() error { return e.cause }

type scanAttempt struct {
	id         ScanAttemptID
	generation DirectoryGeneration
	directory  DirectoryID
	previous   ScanAttemptID
	ctx        context.Context
	done       chan struct{}
	cancel     context.CancelFunc
	waiters    int
	completed  bool
	committed  CommittedDirectory
	err        error
	retryable  bool
	retryAt    time.Time
	failure    *DirectoryFailure
	admission  *BudgetReservation
	resources  *attemptResourceMeter
	progress   uint64
	observers  map[*scanProgressSubscription]struct{}
}

type scanProgressSubscription struct {
	ctx      context.Context
	cancel   context.CancelCauseFunc
	observer ScanProgressObserver
	updates  chan ScanProgress
	done     chan struct{}
	close    sync.Once
}

func newScanProgressSubscription(
	parent context.Context,
	observer ScanProgressObserver,
) *scanProgressSubscription {
	if observer == nil {
		return nil
	}
	ctx, cancel := context.WithCancelCause(parent)
	subscription := &scanProgressSubscription{
		ctx: ctx, cancel: cancel, observer: observer,
		updates: make(chan ScanProgress, 1), done: make(chan struct{}),
	}
	go subscription.run()
	return subscription
}

func (subscription *scanProgressSubscription) run() {
	defer close(subscription.done)
	for {
		select {
		case <-subscription.ctx.Done():
			return
		case progress, ok := <-subscription.updates:
			if !ok {
				return
			}
			if err := subscription.observer.ObserveScanProgress(subscription.ctx, progress); err != nil {
				subscription.cancel(err)
				return
			}
		}
	}
}

// enqueueLatest is called while CatalogStore.mu excludes both publisher and
// unsubscriber races. Replacing a stale milestone preserves monotonicity while
// keeping scan work independent from one receiver's writer capacity.
func (subscription *scanProgressSubscription) enqueueLatest(progress ScanProgress) {
	if subscription == nil {
		return
	}
	select {
	case subscription.updates <- progress:
		return
	default:
	}
	select {
	case <-subscription.updates:
	default:
	}
	select {
	case subscription.updates <- progress:
	default:
	}
}

func (subscription *scanProgressSubscription) stop(drain bool, cause error) {
	if subscription == nil {
		return
	}
	subscription.close.Do(func() {
		if !drain {
			subscription.cancel(cause)
		}
		close(subscription.updates)
	})
}

func finishScanProgressSubscription(subscription *scanProgressSubscription) error {
	if subscription == nil {
		return nil
	}
	<-subscription.done
	cause := context.Cause(subscription.ctx)
	subscription.cancel(context.Canceled)
	return cause
}

func scanProgressDone(subscription *scanProgressSubscription) <-chan struct{} {
	if subscription == nil {
		return nil
	}
	return subscription.done
}

func scanProgressCause(subscription *scanProgressSubscription) error {
	if subscription == nil {
		return nil
	}
	return context.Cause(subscription.ctx)
}

func (s *CatalogStore) subscribeAttemptLocked(
	ctx context.Context,
	attempt *scanAttempt,
	observer ScanProgressObserver,
) *scanProgressSubscription {
	subscription := newScanProgressSubscription(ctx, observer)
	if subscription == nil {
		return nil
	}
	attempt.observers[subscription] = struct{}{}
	if attempt.progress > 0 {
		subscription.enqueueLatest(ScanProgress{
			AttemptID: attempt.id, DiscoveredEntries: attempt.progress,
		})
	}
	return subscription
}

func (s *CatalogStore) detachProgressLocked(
	attempt *scanAttempt,
	subscription *scanProgressSubscription,
	drain bool,
	cause error,
) {
	if subscription == nil {
		return
	}
	delete(attempt.observers, subscription)
	subscription.stop(drain, cause)
}

func (s *CatalogStore) publishAttemptProgress(attempt *scanAttempt, count uint64, force bool) {
	if count == 0 || (!force && count != 1 && count%ScanProgressEntryInterval != 0) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || attempt.completed || s.attempts[attempt.directory] != attempt || count <= attempt.progress {
		return
	}
	attempt.progress = count
	progress := ScanProgress{AttemptID: attempt.id, DiscoveredEntries: count}
	for subscription := range attempt.observers {
		subscription.enqueueLatest(progress)
	}
}
