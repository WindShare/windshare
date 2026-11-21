package account

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"time"
)

type refreshScope uint8

const (
	refreshInactive refreshScope = iota
	refreshExisting
	refreshWithSetup
)

func (service *Service) refreshScopeLocked() refreshScope {
	if service.ctx.Err() != nil {
		return refreshInactive
	}
	for share := range service.officialShares {
		// A share retaining a previous login refreshes that credential on demand.
		if share.credential == nil || sameIdentity(share.credential.record, service.record) {
			return refreshWithSetup
		}
	}
	if service.accountViews != 0 {
		return refreshExisting
	}
	return refreshInactive
}

func (service *Service) refreshScope() refreshScope {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.refreshScopeLocked()
}

func (service *Service) refreshDemandChangedLocked() {
	scope := service.refreshScopeLocked()
	slog.Debug("account_refresh_demand_changed", "official_shares", len(service.officialShares),
		"account_views", service.accountViews, "active", scope != refreshInactive, "automatic_setup", scope == refreshWithSetup)
	service.notifyLocked()
	service.wakeRefreshLocked()
}

func (service *Service) wakeRefreshLocked() {
	select {
	case service.wake <- struct{}{}:
	default:
	}
}

const persistentStoreFailureThreshold = 2

type refreshSchedule struct {
	observed      Record
	next          time.Time
	retry         time.Duration
	storeFailures int
}

func (service *Service) run() {
	schedule := refreshSchedule{retry: MinimumRetry}
	timer := time.NewTimer(PollInterval)
	timer.Stop()
	defer timer.Stop()
	var due <-chan time.Time
	for {
		select {
		case <-service.ctx.Done():
			return
		case <-service.wake:
		case <-due:
		}
		timer.Stop()
		due = nil

		if service.refreshScope() == refreshInactive {
			continue
		}
		if service.lockOperation(service.ctx) != nil {
			return
		}
		service.pollLocked(service.ctx, &schedule)
		<-service.operation
		// Keep deadlines across idle periods so reopening respects backoff.
		if service.refreshScope() != refreshInactive {
			delay := PollInterval
			if !schedule.next.IsZero() {
				delay = min(PollInterval, max(MinimumRetry, schedule.next.Sub(service.config.Now())))
			}
			timer.Reset(delay)
			due = timer.C
		}
	}
}

// The command lock spans the failure transition so a late poll cannot
// replace a newer interactive registration state.
func (service *Service) pollLocked(ctx context.Context, schedule *refreshSchedule) {
	service.mu.Lock()
	scope, beforeRead := service.refreshScopeLocked(), service.record
	service.mu.Unlock()
	if scope == refreshInactive {
		return
	}
	read := service.load
	if scope == refreshWithSetup {
		read = service.loadOrInitialize
	}
	record, err := service.syncSelection(ctx, read, nil)
	if err != nil {
		service.handleLocalPollFailure(schedule, beforeRead, err)
		return
	}
	schedule.storeFailures = 0
	if scope == refreshExisting && record.Credential == nil {
		if !schedule.next.IsZero() {
			state, reason := service.cachedState(record)
			service.transition(record, state, reason, time.Time{})
		}
		*schedule = refreshSchedule{retry: MinimumRetry}
		return
	}
	now := service.config.Now()
	service.observeRefresh(schedule, record, now)
	if now.Before(schedule.next) {
		return
	}
	// Recheck after the store read. A view can disappear or a share can finish
	// while the worker waits for local state.
	scope = service.refreshScope()
	if scope == refreshInactive || scope == refreshExisting && record.Credential == nil {
		return
	}
	if record.Credential != nil && record.Denial == "" && !service.validTicket(record) {
		service.transition(record, Refreshing, "", time.Time{})
	}
	_, err = service.refreshRecord(ctx, record)
	service.scheduleNextRefresh(schedule, record, err)
}

func (service *Service) handleLocalPollFailure(schedule *refreshSchedule, record Record, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		schedule.storeFailures = 0
		service.scheduleNextRefresh(schedule, record, err)
		return
	}
	schedule.storeFailures++
	if isLocalStoreError(err) || schedule.storeFailures >= persistentStoreFailureThreshold {
		service.mu.Lock()
		service.recordStorageErrorLocked(err)
		service.mu.Unlock()
		schedule.retry = MinimumRetry
		schedule.next = service.config.Now().Add(PollInterval)
		return
	}
	service.scheduleNextRefresh(schedule, record, err)
}

func (service *Service) scheduleNextRefresh(schedule *refreshSchedule, record Record, err error) {
	delay := RefreshInterval
	if err != nil {
		delay = service.failure(record, err, schedule.retry)
		schedule.retry = min(schedule.retry*2, MaximumRetry)
	} else {
		schedule.retry = MinimumRetry
	}
	schedule.next = service.config.Now().Add(delay)
}

func (service *Service) observeRefresh(schedule *refreshSchedule, record Record, now time.Time) {
	previous := schedule.observed
	schedule.observed = record
	newIdentity := !sameIdentity(previous, record)
	newGeneration := previous.Certificate != record.Certificate
	if newIdentity || newGeneration {
		schedule.next, schedule.retry = time.Time{}, MinimumRetry
	}
	if record.Credential == nil {
		if !reflect.DeepEqual(previous.Proof, record.Proof) {
			schedule.next = time.Time{}
		}
		return
	}
	// Profile edits are independent of ticket freshness and retry backoff.
	if !newIdentity && !newGeneration && previous.Ticket == record.Ticket && previous.Denial == record.Denial {
		return
	}
	if next := service.cachedRefreshAt(record, now); next.After(now) {
		schedule.next, schedule.retry = next, MinimumRetry
		service.transition(record, Ready, "", next)
		slog.Debug("account_refresh_satisfied_by_ticket", "credential", record.Credential.ID, "refresh_at", next)
	}
}

func (service *Service) cachedRefreshAt(record Record, now time.Time) time.Time {
	ticket, valid := service.usableTicket(record, now)
	if !valid {
		return time.Time{}
	}
	// A signer ahead of the local clock must not lengthen the refresh cadence.
	issuedAt := time.Unix(int64(ticket.IssuedAt), 0)
	if issuedAt.After(now) {
		issuedAt = now
	}
	next := issuedAt.Add(RefreshInterval)
	for _, cutoff := range []time.Time{
		time.Unix(int64(ticket.ExpiresAt), 0).Add(-TicketMargin),
		time.Unix(int64(ticket.Entitlement.ValidUntil), 0),
	} {
		if cutoff.Before(next) {
			next = cutoff
		}
	}
	return next
}

// RefreshOnce runs one explicit attempt without activating background maintenance.
// A usable cached ticket can coexist with an unfinished or failed attempt.
func (service *Service) RefreshOnce(ctx context.Context) (RefreshOutcome, error) {
	if service == nil {
		return "", ErrNotConfigured
	}
	// Bound the wait for the operation and the store as well as the request.
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	if err := service.lockOperation(ctx); err != nil {
		return "", err
	}
	defer func() { <-service.operation }()
	record, err := service.syncRecord(ctx)
	if err != nil {
		return "", err
	}
	logger := slog.Default()
	if record.Credential != nil {
		logger = logger.With("credential", record.Credential.ID)
	}
	outcome, err := service.refreshRecord(ctx, record)
	logger.DebugContext(ctx, "account_refresh_attempt_completed", "outcome", outcome, "error", err)
	return outcome, err
}

func (service *Service) syncRecord(ctx context.Context) (Record, error) {
	return service.syncSelection(ctx, service.loadOrInitialize, nil)
}

func (service *Service) loadOrInitialize(ctx context.Context) (Record, error) {
	record, err := service.load(ctx)
	// The CLI and desktop must agree on the setup secret before either contacts the authority.
	if err == nil && record.Credential == nil && record.SetupSecret == "" {
		return service.initialize(ctx)
	}
	return record, err
}

func (service *Service) refreshRecord(ctx context.Context, record Record) (RefreshOutcome, error) {
	if record.Credential != nil {
		if err := service.requestIssuance(ctx, record, "/v1/accounts/ticket", record.Credential.Secret, struct{}{}); err != nil {
			return "", err
		}
		return RefreshSucceeded, nil
	}
	proof, err := service.pendingRegistration(ctx, record)
	if err != nil {
		return "", err
	}
	if proof == nil {
		return RefreshSucceeded, nil
	}
	state := proof.Challenge.requiredState()
	service.transition(record, state, "", time.Time{})
	if record.Denial != "" {
		return "", &fault{Kind: "denied", Reason: record.Denial}
	}
	if state == EmailRequired {
		return RefreshNeedsEmail, nil
	}
	return RefreshNeedsRegistration, nil
}

func (service *Service) requestIssuance(ctx context.Context, record Record, path, secret string, request any) error {
	var result session
	err := service.call(ctx, path, secret, request, &result)
	if err == nil {
		return service.install(ctx, record, result, false)
	}
	failure, ok := errors.AsType[*fault](err)
	if !ok || failure.Kind != "denied" {
		return err
	}
	storeErr := service.recordDenial(ctx, record, failure.Reason, func(current Record) (Record, error) {
		if !sameIdentity(current, record) {
			return Record{}, ErrSuperseded
		}
		current.Denial = failure.Reason
		return current, nil
	})
	return errors.Join(err, storeErr)
}

func (service *Service) saveProof(ctx context.Context, previous Record, proof *Proof) error {
	return service.updateProof(ctx, previous, func(current *Proof) (*Proof, error) {
		// Another process can replace the challenge while a request is in flight.
		if !reflect.DeepEqual(current, previous.Proof) {
			return nil, ErrSuperseded
		}
		return proof, nil
	})
}

func (service *Service) updateProof(ctx context.Context, previous Record, update func(*Proof) (*Proof, error)) error {
	if err := service.lockLocal(ctx); err != nil {
		return err
	}
	defer func() { <-service.local }()
	record, err := service.config.Store.Update(ctx, func(current Record) (Record, error) {
		if !sameIdentity(current, previous) {
			return Record{}, ErrSuperseded
		}
		proof, err := update(current.Proof)
		if err != nil {
			return Record{}, err
		}
		current.Proof = proof
		return current, nil
	})
	if err == nil {
		service.mu.Lock()
		service.adoptLocked(record)
		service.notifyLocked()
		service.mu.Unlock()
	}
	return err
}

func (service *Service) failure(record Record, err error, delay time.Duration) time.Duration {
	state := Unavailable
	reason := err.Error()
	if remote, ok := errors.AsType[*fault](err); ok {
		reason = remote.Reason
		if remote.Kind == "denied" {
			state, delay = Denied, MaximumRetry
		}
	}
	delay = rateLimitDelay(delay, err)
	service.transition(record, state, reason, service.config.Now().Add(delay))
	return delay
}

func rateLimitDelay(delay time.Duration, err error) time.Duration {
	if remote, ok := errors.AsType[*fault](err); ok && remote.Kind == "rate_limited" {
		return max(delay, time.Duration(min(remote.RetryAfter, maxRateLimitSeconds))*time.Second)
	}
	return delay
}
