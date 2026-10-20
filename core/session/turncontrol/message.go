// Package turncontrol defines session-authenticated TURN delegation messages.
// It contains wire values only; issuers, sockets and account policy stay outside core.
package turncontrol

import (
	"errors"
	"time"
)

const (
	Schema                    = uint64(3)
	IdentityBytes             = 16
	MaximumBodyBytes          = 8192
	MaximumTextBytes          = 1024
	MaximumIdentifierBytes    = 64
	MaximumURLs               = 8
	MaximumURLBytes           = 512
	MaximumDemandLifetime     = 120 * time.Second
	MaximumCredentialLifetime = 48 * time.Hour
)

var ErrMessage = errors.New("invalid TURN path control")

type Header struct {
	SessionID   [IdentityBytes]byte
	OwnerPathID [IdentityBytes]byte
	Sequence    uint64
}

type Message interface {
	Header() Header
	turnMessage()
}

type Demand struct {
	Authority Header
	HoldFor   time.Duration
}

func (m Demand) Header() Header { return m.Authority }
func (Demand) turnMessage()     {}

type CredentialKind uint8

const (
	Fixed CredentialKind = iota + 1
	Expiring
)

// Fixed passwords have no WindShare-enforced expiry. Expiring grants carry a
// remaining duration, so receiver clock skew cannot postpone replacement.
type Grant struct {
	Authority                  Header
	PathID                     [IdentityBytes]byte
	Replaces                   [IdentityBytes]byte
	Provider, Account, Profile string
	CredentialKind             CredentialKind
	Username, Password         string
	ValidFor                   time.Duration
	URLs                       []string
}

func (m Grant) Header() Header { return m.Authority }
func (Grant) turnMessage()     {}

type Ready struct {
	Authority Header
	PathID    [IdentityBytes]byte
}

func (m Ready) Header() Header { return m.Authority }
func (Ready) turnMessage()     {}

type RetireReason uint8

const (
	Idle RetireReason = iota + 1
	Replaced
	Expired
	BudgetExhausted
	ProviderRevoked
	Unhelpful
)

type Retire struct {
	Authority Header
	PathID    [IdentityBytes]byte
	Reason    RetireReason
}

func (m Retire) Header() Header { return m.Authority }
func (Retire) turnMessage()     {}

type Unavailable struct {
	Authority  Header
	Profile    string
	RetryAfter time.Duration
	Reason     string
}

func (m Unavailable) Header() Header { return m.Authority }
func (Unavailable) turnMessage()     {}

type Available struct {
	Authority Header
	Profiles  []string
}

func (m Available) Header() Header { return m.Authority }
func (Available) turnMessage()     {}

// Report carries receiver-confirmed useful content, not a billing assertion.
// The cumulative counter makes retries on surviving control lanes idempotent.
type Report struct {
	Authority   Header
	PathID      [IdentityBytes]byte
	UsefulBytes uint64
}

func (m Report) Header() Header { return m.Authority }
func (Report) turnMessage()     {}
