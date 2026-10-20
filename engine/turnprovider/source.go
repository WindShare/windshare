// Package turnprovider implements credential issuers. Account policy, demand,
// renewal scheduling and transfer budgets belong to the Engine owner.
package turnprovider

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

type Issued struct {
	Credentials iceconfig.Credentials
	URLs        []string
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

func (source *Static) Issue(ctx context.Context, _ Subject) (Issued, error) {
	if err := ctx.Err(); err != nil {
		return Issued{}, err
	}
	if !source.credential.ValidAt(source.now()) {
		return Issued{}, ErrExpired
	}
	return Issued{Credentials: source.credential}, nil
}

func validLifetime(lifetime time.Duration) bool {
	return lifetime >= time.Minute && lifetime <= MaximumLifetime && lifetime%time.Second == 0
}
