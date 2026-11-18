package account

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

// authorityChallengeLifetime matches CHALLENGE_SECONDS in the authority's challenge policy.
const authorityChallengeLifetime = 15 * time.Minute

func (service *Service) run() {
	retry := MinimumRetry
	next := service.config.Now()
	for {
		current, err := service.load(service.ctx)
		if err == nil {
			service.mu.Lock()
			service.loaded = true
			current = service.withDenialLocked(current)
			if !reflect.DeepEqual(service.record, current) {
				service.adoptLocked(current)
				next = service.config.Now()
				service.notifyLocked()
			}
			service.mu.Unlock()
		}
		now := service.config.Now()
		if err != nil || !now.Before(next) {
			if err == nil {
				err = service.step(service.ctx)
			}
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
	record, err := service.initialize(ctx)
	if err != nil {
		return err
	}
	service.mu.Lock()
	service.adoptLocked(record)
	record = service.record
	service.mu.Unlock()
	if record.Credential == nil {
		return service.register(ctx, record)
	}
	if record.Denial == "" && !service.validTicket(record) {
		service.transition(Refreshing, "", time.Time{})
	}
	return service.requestIssuance(ctx, record, "/v1/accounts/ticket", record.Credential.Secret, struct{}{})
}

func (service *Service) register(ctx context.Context, record Record) error {
	if record.Proof != nil && uint64(service.config.Now().Unix()) >= record.Proof.Challenge.ExpiresAt {
		if err := service.saveProof(ctx, record, nil); err != nil {
			return err
		}
		record.Proof = nil
	}
	if record.Proof != nil && record.Proof.Solution == "" {
		if record.Proof.Challenge.Mode == "email" {
			service.transition(EmailRequired, "", time.Time{})
			return nil
		}
		proof, err := service.solveProof(ctx, record)
		if err != nil {
			return err
		}
		record.Proof = proof
	}
	_, err := service.submitRegistration(ctx, record, record.Proof)
	return err
}

// submitRegistration installs a session or saves and returns the next challenge.
// Interactive callers need that outcome independently of background account state.
func (service *Service) submitRegistration(ctx context.Context, record Record, proof *Proof) (*Challenge, error) {
	request := struct {
		Secret    string  `json:"client_secret"`
		Challenge *string `json:"challenge"`
		Solution  *string `json:"solution"`
	}{Secret: record.SetupSecret}
	if proof != nil && proof.Solution != "" {
		request.Challenge, request.Solution = &proof.Challenge.ID, &proof.Solution
	}
	err := service.requestIssuance(ctx, record, "/v1/accounts", "", request)
	if failure, ok := errors.AsType[*fault](err); ok && failure.Kind == "verification" {
		if err := service.acceptChallenge(ctx, record, failure.Challenge); err != nil {
			return nil, err
		}
		return &failure.Challenge, nil
	}
	return nil, err
}

func (service *Service) solveProof(ctx context.Context, record Record) (*Proof, error) {
	if record.Proof.Challenge.Mode != "proof_of_work" {
		return nil, errors.New("invalid account challenge")
	}
	service.transition(Verifying, "", time.Time{})
	proof := *record.Proof
	var err error
	proof.Solution, err = service.solve(ctx, proof.Challenge)
	if err != nil {
		return nil, err
	}
	if err := service.saveProof(ctx, record, &proof); err != nil {
		return nil, err
	}
	return &proof, nil
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

func (service *Service) acceptChallenge(ctx context.Context, record Record, challenge Challenge) error {
	if challenge.Mode != "email" && challenge.Mode != "proof_of_work" ||
		!service.acceptsChallengeExpiry(challenge.ExpiresAt) || challenge.Bits > 32 {
		return errors.New("invalid account challenge")
	}
	if err := service.saveProof(ctx, record, &Proof{Challenge: challenge}); err != nil {
		return err
	}
	if challenge.Mode == "email" {
		service.transition(EmailRequired, "", time.Time{})
	} else {
		service.transition(Verifying, "", time.Time{})
		service.Retry()
	}
	return nil
}

// The authority stamps a fresh challenge's expiry with its own clock. A local
// clock running behind it must not refuse every fresh challenge, so the upper
// bound tolerates the same skew as ticket and certificate acceptance.
func (service *Service) acceptsChallengeExpiry(expiresAt uint64) bool {
	now := uint64(service.config.Now().Unix())
	return expiresAt > now && expiresAt <= now+uint64(authorityChallengeLifetime/time.Second)+serviceauthority.ClockSkew
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
