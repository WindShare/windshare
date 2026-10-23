// Package provider implements credential issuers. Account policy, demand,
// renewal scheduling and transfer budgets belong to the Engine owner.
package provider

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

const (
	DefaultLifetime = time.Hour
	MaximumLifetime = 48 * time.Hour
)

var (
	ErrConfiguration = errors.New("invalid TURN issuer configuration")
	ErrExpired       = errors.New("configured TURN credentials have expired")
)

type Subject struct {
	SessionID [16]byte
	PathID    [16]byte
}

// Acquisition owns cleanup independently of usable credentials. Callers must retain
// Cleanup even when issuance returns an error: a lost reply cannot undo remote work.
type Acquisition struct {
	Credentials iceconfig.Credentials
	URLs        []string
	Cleanup     *Cleanup
	remote      *remoteRequest
}

type CleanupResult uint8

const (
	RevocationConfirmed CleanupResult = iota + 1
	RevocationQueued
	IssuanceCanceled
	AuthorityExpired
)

// Cleanup pins the issuing authority and resource identity. ExpiresAt bounds
// responsibility when the issuer remains unreachable; queued cleanup belongs to
// the issuer and must never be reported as provider-confirmed revocation.
type Cleanup struct {
	ID        string
	ExpiresAt time.Time
	Revoke    func(context.Context) (CleanupResult, error)
}

// Optional interfaces are discovered at the consumer. Static passwords and
// coturn's shared-secret algorithm do not promise remote revocation or billing.
type Static struct {
	credential iceconfig.Credentials
	now        func() time.Time
}

func NewStatic(credential iceconfig.Credentials, now func() time.Time) (*Static, error) {
	if credential.Kind() != iceconfig.FixedCredential && credential.Kind() != iceconfig.ExpiringCredential {
		return nil, ErrConfiguration
	}
	if now == nil {
		now = time.Now
	}
	return &Static{credential: credential, now: now}, nil
}

func (source *Static) Issue(ctx context.Context, _ Subject) (Acquisition, error) {
	if err := ctx.Err(); err != nil {
		return Acquisition{}, err
	}
	if !source.credential.ValidAt(source.now()) {
		return Acquisition{}, ErrExpired
	}
	return Acquisition{Credentials: source.credential}, nil
}

func validLifetime(lifetime time.Duration) bool {
	return lifetime >= time.Minute && lifetime <= MaximumLifetime && lifetime%time.Second == 0
}
