// Package turnpath owns session-scoped delegated TURN paths. It consumes account
// authority from the application without knowing how a provider issues secrets.
package turnpath

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

type Profile struct {
	Identity iceconfig.Identity
}
type Subject struct {
	SessionID [16]byte
	PathID    [16]byte
}

// Inventory is one atomic configuration view. Changed closes when a newer view
// is available, so a sender cannot miss an update between reading and waiting.
type Inventory struct {
	Revision uint64
	Profiles []Profile
	Changed  <-chan struct{}
}

var (
	ErrSuperseded = errors.New("TURN configuration superseded")
	ErrBudget     = errors.New("TURN account local usage budget exhausted")
)

type Accounts interface {
	Inventory() Inventory
	Acquire(context.Context, string, Subject) (*Lease, error)
}

type Usage struct {
	// Payload is accepted application traffic, including protocol framing.
	// Useful is separately reported completed content, excluding retries.
	Payload, Useful, TransportSent, TransportReceived uint64
}

type LeaseConfig struct {
	Relay     iceconfig.Relay
	Renew     func(context.Context, Subject) (*Lease, error)
	Authority context.Context
	Charge    func(Usage) bool
	Release   func()
}

// Lease binds delegated credentials to local account capacity. Releasing it
// stops local authority; provider-confirmed revocation is an account observation.
type Lease struct {
	config LeaseConfig
	once   sync.Once
}

func NewLease(config LeaseConfig) *Lease { return &Lease{config: config} }
func (l *Lease) Relay() iceconfig.Relay  { return l.config.Relay }
func (l *Lease) Renewable() bool         { return l.config.Renew != nil }
func (l *Lease) Renew(ctx context.Context, subject Subject) (*Lease, error) {
	if l.config.Renew == nil {
		return nil, errors.New("TURN credential cannot be renewed")
	}
	return l.config.Renew(ctx, subject)
}
func (l *Lease) Lost() <-chan struct{} {
	if l.config.Authority == nil {
		return nil
	}
	return l.config.Authority.Done()
}
func (l *Lease) Loss() error {
	if l.config.Authority == nil {
		return nil
	}
	return context.Cause(l.config.Authority)
}
func (l *Lease) Charge(usage Usage) bool {
	return l.config.Charge == nil || l.config.Charge(usage)
}
func (l *Lease) Close() {
	l.once.Do(func() {
		if l.config.Release != nil {
			l.config.Release()
		}
	})
}
