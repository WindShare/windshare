package senderrelay

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/serviceauthority"
)

// AuthorizationSource performs no network I/O. A snapshot and its change signal
// belong to the same revision, so setup completion cannot be lost between them.
type AuthorizationSource interface {
	RegistrationAuthorization() Authorization
}

type AuthorizationState = serviceauthority.AdmissionState
type Authorization = serviceauthority.RegistrationAuthorization

const (
	AuthorizationReady            = serviceauthority.AdmissionReady
	AuthorizationPendingSetup     = serviceauthority.AdmissionPendingSetup
	AuthorizationExpired          = serviceauthority.AdmissionExpired
	AuthorizationTemporaryFailure = serviceauthority.AdmissionTemporaryFailure
	AuthorizationDenied           = serviceauthority.AdmissionDenied
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

func (lifecycle *Lifecycle) waitAuthorization(ctx context.Context, attempt uint32) (Authorization, error) {
	var announced AuthorizationState
	for {
		authorization := lifecycle.config.Candidate.authorization()
		if authorization.State == AuthorizationReady {
			if !validBearer(authorization.Bearer) {
				return Authorization{}, ErrCandidate
			}
			return authorization, nil
		}
		failure := &AuthorizationError{State: authorization.State, Cause: authorization.Cause}
		if announced != authorization.State {
			lifecycle.observeRecoveryAttempt(Attempt{ShareInstance: lifecycle.config.Fresh.ShareInstance,
				Number: attempt, State: AttemptAuthorization, Authorization: authorization.State,
				Failure: failure, Terminal: authorization.State == AuthorizationDenied})
			announced = authorization.State
		}
		switch authorization.State {
		case AuthorizationDenied:
			return Authorization{}, failure
		case AuthorizationPendingSetup, AuthorizationExpired, AuthorizationTemporaryFailure:
			if authorization.Changed == nil {
				return Authorization{}, errors.Join(ErrCandidate, errors.New("authorization source has no change signal"))
			}
		default:
			return Authorization{}, ErrCandidate
		}
		select {
		case <-ctx.Done():
			return Authorization{}, ctx.Err()
		case <-authorization.Changed:
		case <-lifecycle.wake:
		}
	}
}
