// Package iceconfig describes immutable ICE inputs. Discovery and delegated
// relay authority have different lifetimes and must never share an endpoint pool.
package iceconfig

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/pion/stun/v4"
)

const (
	MaximumURLs        = 8
	MaximumURLBytes    = 512
	MaximumSecretBytes = 1024
	MaximumIDBytes     = 64
)

var ErrConfiguration = errors.New("invalid ICE connection configuration")

type Identity struct {
	Provider string
	Account  string
	Profile  string
}

func (id Identity) Validate() error {
	for _, value := range []string{id.Provider, id.Account, id.Profile} {
		if value == "" || len(value) > MaximumIDBytes {
			return ErrConfiguration
		}
		for _, char := range value {
			if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
				char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' {
				continue
			}
			return ErrConfiguration
		}
	}
	return nil
}

type CredentialKind uint8

const (
	FixedCredential CredentialKind = iota + 1
	ExpiringCredential
)

// Credentials can only be constructed with a known lifetime. The zero value
// cannot accidentally turn a missing expiry into a permanent password.
type Credentials struct {
	kind     CredentialKind
	username string
	password string
	expires  time.Time
}

func Fixed(username, password string) (Credentials, error) {
	return credentials(FixedCredential, username, password, time.Time{})
}

func Expiring(username, password string, expires time.Time) (Credentials, error) {
	if expires.IsZero() || expires.UnixMilli() <= 0 {
		return Credentials{}, ErrConfiguration
	}
	return credentials(ExpiringCredential, username, password, expires)
}

func credentials(kind CredentialKind, username, password string, expires time.Time) (Credentials, error) {
	if username == "" || password == "" || len(username) > MaximumSecretBytes || len(password) > MaximumSecretBytes {
		return Credentials{}, ErrConfiguration
	}
	return Credentials{kind: kind, username: username, password: password, expires: expires}, nil
}

func (c Credentials) Kind() CredentialKind { return c.kind }
func (c Credentials) Username() string     { return c.username }
func (c Credentials) Password() string     { return c.password }
func (c Credentials) ExpiresAt() time.Time { return c.expires }
func (c Credentials) ValidAt(now time.Time) bool {
	return c.kind == FixedCredential || c.kind == ExpiringCredential && now.Before(c.expires)
}

// Relay is one service, whose URLs are alternatives, not separate traffic lanes.
// Pion's pinned TURN gatherer supports IPv4 allocations only; IPv6 discovery
// remains available on the independent direct path.
type Relay struct {
	identity    Identity
	urls        []string
	credentials Credentials
}

func NewRelay(identity Identity, urls []string, credential Credentials) (Relay, error) {
	if identity.Validate() != nil || len(urls) == 0 || len(urls) > MaximumURLs ||
		(credential.kind != FixedCredential && credential.kind != ExpiringCredential) {
		return Relay{}, ErrConfiguration
	}
	seen := make(map[string]bool, len(urls))
	for _, raw := range urls {
		if ValidateRelayURL(raw) != nil || seen[raw] {
			return Relay{}, ErrConfiguration
		}
		seen[raw] = true
	}
	return Relay{identity: identity, urls: slices.Clone(urls), credentials: credential}, nil
}

func ValidateRelayURL(raw string) error {
	if raw == "" || len(raw) > MaximumURLBytes || strings.ContainsAny(raw, " \t\r\n#@/") {
		return ErrConfiguration
	}
	uri, err := stun.ParseURI(raw)
	if err != nil || uri.Host == "" || uri.Port <= 0 || uri.Port > 65535 ||
		(uri.Scheme != stun.SchemeTypeTURN && uri.Scheme != stun.SchemeTypeTURNS) ||
		strings.Contains(uri.Host, ":") ||
		(uri.Scheme == stun.SchemeTypeTURNS && uri.Proto != stun.ProtoTypeTCP) {
		return ErrConfiguration
	}
	return nil
}

func (r Relay) Identity() Identity       { return r.identity }
func (r Relay) URLs() []string           { return slices.Clone(r.urls) }
func (r Relay) Credentials() Credentials { return r.credentials }

type CandidatePolicy uint8

const (
	AllCandidates CandidatePolicy = iota
	RelayCandidates
)

// Connection is a snapshot consumed by one gathering generation. Replacing a
// credential requires a new connection; mutating live Pion configuration does
// not refresh an existing allocation.
type Connection struct {
	discovery []string
	relay     *Relay
	policy    CandidatePolicy
}

func New(discovery []string, relay *Relay, policy CandidatePolicy) (Connection, error) {
	if len(discovery) > MaximumURLs || policy > RelayCandidates || policy == RelayCandidates && relay == nil {
		return Connection{}, ErrConfiguration
	}
	for _, raw := range discovery {
		uri, err := stun.ParseURI(raw)
		if len(raw) > MaximumURLBytes || err != nil || uri.Scheme != stun.SchemeTypeSTUN {
			return Connection{}, ErrConfiguration
		}
	}
	result := Connection{discovery: slices.Clone(discovery), policy: policy}
	if relay != nil {
		value, err := NewRelay(relay.identity, relay.urls, relay.credentials)
		if err != nil {
			return Connection{}, err
		}
		result.relay = &value
	}
	return result, nil
}

func (c Connection) DiscoveryURLs() []string { return slices.Clone(c.discovery) }
func (c Connection) Policy() CandidatePolicy { return c.policy }
func (c Connection) Relay() (Relay, bool) {
	if c.relay == nil {
		return Relay{}, false
	}
	value := *c.relay
	value.urls = slices.Clone(value.urls)
	return value, true
}
