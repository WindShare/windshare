package senderrelay

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/serviceauthority"
)

// AuthorizationSource is consulted for fresh registration and performs no
// network I/O. A waiting result and its change signal describe one revision.
type AuthorizationSource interface {
	RegistrationAuthorization() Authorization
}

type AuthorizationState = serviceauthority.AdmissionState
type Authorization = serviceauthority.RegistrationAuthorization

const (
	AuthorizationReady              = serviceauthority.AdmissionReady
	AuthorizationPendingSetup       = serviceauthority.AdmissionPendingSetup
	AuthorizationExpired            = serviceauthority.AdmissionExpired
	AuthorizationTemporaryFailure   = serviceauthority.AdmissionTemporaryFailure
	AuthorizationDenied             = serviceauthority.AdmissionDenied
	AuthorizationUserActionRequired = serviceauthority.AdmissionUserActionRequired
)

type StaticKey string

func (key StaticKey) RegistrationAuthorization() Authorization {
	return Authorization{State: AuthorizationReady, Bearer: string(key)}
}

type AuthorizationError struct {
	State AuthorizationState
	Cause error
}

func (err *AuthorizationError) Error() string {
	return fmt.Sprintf("registration authorization %d: %v", err.State, err.Cause)
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

func (candidate Candidate) authorization() Authorization {
	if candidate.Authorization == nil {
		return StaticKey("").RegistrationAuthorization()
	}
	return candidate.Authorization.RegistrationAuthorization()
}

func (lifecycle *Lifecycle) acquireRegistrationAttempt(ctx context.Context, resume bool, attempt uint32) (AttemptPermit, Authorization, error) {
	for {
		permit, err := lifecycle.beginAttempt(ctx)
		if err != nil || resume {
			return permit, Authorization{}, err
		}
		// A candidate can wait for a slot longer than a ticket is valid.
		authorization := lifecycle.config.Candidate.authorization()
		if authorization.State == AuthorizationReady && validBearer(authorization.Bearer) {
			return permit, authorization, nil
		}
		// Setup must leave capacity available to other paths.
		if permit != nil {
			permit.Release()
		}
		if authorization.State == AuthorizationReady {
			return nil, Authorization{}, ErrCandidate
		}
		if err := lifecycle.waitAuthorization(ctx, attempt, authorization); err != nil {
			return nil, Authorization{}, err
		}
	}
}

func (lifecycle *Lifecycle) waitAuthorization(ctx context.Context, attempt uint32, authorization Authorization) error {
	var announced AuthorizationState
	for authorization.State != AuthorizationReady {
		failure := &AuthorizationError{State: authorization.State, Cause: authorization.Cause}
		if announced != authorization.State {
			lifecycle.showAuthorizationWait(authorization.State == AuthorizationUserActionRequired)
			lifecycle.observeRecoveryAttempt(Attempt{ShareInstance: lifecycle.config.Fresh.ShareInstance,
				Number: attempt, State: AttemptAuthorization, Authorization: authorization.State,
				Failure: failure, Terminal: authorization.State == AuthorizationDenied})
			announced = authorization.State
		}
		switch authorization.State {
		case AuthorizationDenied:
			return failure
		case AuthorizationPendingSetup, AuthorizationExpired, AuthorizationTemporaryFailure, AuthorizationUserActionRequired:
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
		authorization = lifecycle.config.Candidate.authorization()
	}
	lifecycle.showAuthorizationWait(false)
	return nil
}
