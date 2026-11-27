package relayset

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/serviceauthority"
)

// AuthorizationSource reserves authorization for one fresh registration. Waiting
// results describe the revision to watch and hold no reservation or dial slot.
type AuthorizationSource interface {
	ReserveRegistrationAuthorization() Authorization
}

type AuthorizationState = serviceauthority.AdmissionState
type Authorization = serviceauthority.RegistrationAuthorization

const (
	AuthorizationReady              = serviceauthority.AdmissionReady
	AuthorizationPendingSetup       = serviceauthority.AdmissionPendingSetup
	AuthorizationPendingAttribution = serviceauthority.AdmissionPendingAttribution
	AuthorizationExpired            = serviceauthority.AdmissionExpired
	AuthorizationTemporaryFailure   = serviceauthority.AdmissionTemporaryFailure
	AuthorizationDenied             = serviceauthority.AdmissionDenied
	AuthorizationUserActionRequired = serviceauthority.AdmissionUserActionRequired
)

type StaticKey string

func (key StaticKey) ReserveRegistrationAuthorization() Authorization {
	return Authorization{State: AuthorizationReady, Bearer: string(key)}
}

type AuthorizationError struct {
	State AuthorizationState
	Cause error
}

func (err *AuthorizationError) Error() string {
	state := err.State.Name()
	if state == "" {
		state = fmt.Sprintf("unknown(%d)", err.State)
	}
	if err.Cause == nil {
		return "registration authorization " + state
	}
	return fmt.Sprintf("registration authorization %s: %v", state, err.Cause)
}

func (err *AuthorizationError) Unwrap() error { return err.Cause }

func validBearer(token string) bool {
	if len(token) > MaxAccessKeyBytes {
		return false
	}
	for _, character := range token {
		if character < '!' || character > '~' {
			return false
		}
	}
	return true
}

func (candidate Candidate) reserveAuthorization() Authorization {
	if candidate.Authorization == nil {
		return StaticKey("").ReserveRegistrationAuthorization()
	}
	return candidate.Authorization.ReserveRegistrationAuthorization()
}

func (lifecycle *SenderLifecycle) acquireRegistrationAttempt(ctx context.Context, resume bool, attempt uint32) (AttemptPermit, Authorization, error) {
	var announced AuthorizationState
	for {
		permit, err := lifecycle.beginAttempt(ctx)
		if err != nil || resume {
			return permit, Authorization{}, err
		}
		// A candidate can wait for a slot longer than a ticket is valid.
		authorization := lifecycle.config.Candidate.reserveAuthorization()
		if authorization.State == AuthorizationReady && validBearer(authorization.Bearer) {
			if announced != 0 {
				lifecycle.showAuthorizationWait(RegistrationPending)
			}
			return permit, authorization, nil
		}
		if permit != nil {
			permit.Release()
		}
		if authorization.State == AuthorizationReady {
			authorization.Reservation.Finish(serviceauthority.RegistrationNotSent)
			return nil, Authorization{}, ErrCandidate
		}
		if announced != authorization.State {
			lifecycle.observeAuthorizationWait(attempt, authorization)
			announced = authorization.State
		}
		// Allow another route to dial while account or attribution state changes.
		if err := lifecycle.waitAuthorization(ctx, authorization); err != nil {
			return nil, Authorization{}, err
		}
	}
}

func (lifecycle *SenderLifecycle) observeAuthorizationWait(attempt uint32, authorization Authorization) {
	state := RegistrationWaitingAuthorization
	if authorization.State == AuthorizationUserActionRequired {
		state = RegistrationNeedsUserAuthorization
	}
	lifecycle.showAuthorizationWait(state)
	lifecycle.observeRecoveryAttempt(Attempt{ShareInstance: lifecycle.config.Fresh.ShareInstance,
		Number: attempt, State: AttemptAuthorization, Authorization: authorization.State,
		Failure:  &AuthorizationError{State: authorization.State, Cause: authorization.Cause},
		Terminal: authorization.State == AuthorizationDenied})
}

func (lifecycle *SenderLifecycle) waitAuthorization(ctx context.Context, authorization Authorization) error {
	switch authorization.State {
	case AuthorizationDenied:
		return &AuthorizationError{State: authorization.State, Cause: authorization.Cause}
	case AuthorizationPendingSetup, AuthorizationPendingAttribution, AuthorizationExpired, AuthorizationTemporaryFailure, AuthorizationUserActionRequired:
		if authorization.Changed == nil {
			return errors.Join(ErrCandidate, errors.New("authorization source has no change signal"))
		}
	default:
		return ErrCandidate
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-authorization.Changed:
	case <-lifecycle.wake:
	}
	return nil
}
