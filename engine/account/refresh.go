package account

import (
	"context"
	"errors"
	"reflect"
	"time"
)

func (service *Service) run() {
	retry := MinimumRetry
	next := service.config.Now()
	for service.lockOperation(service.ctx) == nil {
		if updated, err := service.refreshLocked(service.ctx, next); updated || err != nil {
			delay := RefreshInterval
			if err != nil {
				delay = retry
				retry = min(retry*2, MaximumRetry)
				delay = service.failure(err, delay)
			} else {
				retry = MinimumRetry
			}
			next = service.config.Now().Add(delay)
		}
		<-service.operation
		timer := time.NewTimer(min(PollInterval, max(MinimumRetry, next.Sub(service.config.Now()))))
		select {
		case <-service.ctx.Done():
			timer.Stop()
			return
		case <-service.wake:
			timer.Stop()
			next = service.config.Now()
		case <-timer.C:
		}
	}
}

func (service *Service) step(ctx context.Context) error {
	if err := service.lockOperation(ctx); err != nil {
		return err
	}
	defer func() { <-service.operation }()
	_, err := service.refreshLocked(ctx, time.Time{})
	return err
}

// The caller owns the command lock through the failure transition. A late poll
// or retry must never replace a newer interactive registration state.
func (service *Service) refreshLocked(ctx context.Context, next time.Time) (bool, error) {
	record, err := service.load(ctx)
	if err != nil {
		return false, err
	}
	// The CLI and desktop must agree on the setup secret before either contacts the authority.
	if record.Credential == nil && record.SetupSecret == "" {
		record, err = service.initialize(ctx)
		if err != nil {
			return false, err
		}
	}
	service.mu.Lock()
	service.loaded = true
	record = service.withDenialLocked(record)
	changed := !reflect.DeepEqual(service.record, record)
	if changed {
		service.adoptLocked(record)
		service.notifyLocked()
	}
	record = service.record
	service.mu.Unlock()
	if !changed && service.config.Now().Before(next) {
		return false, nil
	}
	if record.Credential == nil {
		proof, err := service.pendingRegistration(ctx, record)
		if proof != nil {
			service.transition(proof.Challenge.requiredState(), "", time.Time{})
		}
		return true, err
	}
	if record.Denial == "" && !service.validTicket(record) {
		service.transition(Refreshing, "", time.Time{})
	}
	return true, service.requestIssuance(ctx, record, "/v1/accounts/ticket", record.Credential.Secret, struct{}{})
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
	service.mu.Lock()
	service.rememberDenialLocked(record, failure.Reason)
	service.mu.Unlock()
	_, storeErr := service.config.Store.Update(ctx, func(current Record) (Record, error) {
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

func (service *Service) failure(err error, delay time.Duration) time.Duration {
	state := Unavailable
	reason := err.Error()
	if remote, ok := errors.AsType[*fault](err); ok {
		reason = remote.Reason
		if remote.Kind == "denied" {
			state, delay = Denied, MaximumRetry
		}
		if remote.Kind == "rate_limited" {
			delay = max(delay, time.Duration(min(remote.RetryAfter, 3600))*time.Second)
		}
	}
	service.transition(state, reason, service.config.Now().Add(delay))
	return delay
}
