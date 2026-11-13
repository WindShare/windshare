// Package nodedirectory verifies deployment configuration without performing I/O.
package nodedirectory

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"slices"
	"time"
)

const (
	Format                     = 1
	MaximumEnvelopeBytes       = 512 << 10
	MaximumNodes               = 256
	MaximumPublicSTUN          = 128
	MaximumCounter             = 1<<53 - 1
	DefaultGrace               = 7 * 24 * time.Hour
	MaximumCertificateLifetime = 180 * 24 * time.Hour
	MaximumPayloadLifetime     = 90 * 24 * time.Hour
	ClockSkew                  = 5 * time.Minute
	CertificateDomain          = "windshare/network-certificate/v1\x00"
	PayloadDomain              = "windshare/network-directory/v1\x00"
)

var (
	ErrFormat          = errors.New("invalid network directory")
	ErrSignature       = errors.New("network directory signature rejected")
	ErrExpired         = errors.New("network directory temporarily unavailable")
	ErrRollback        = errors.New("network directory rollback or equivocation")
	ErrAliasReassigned = errors.New("network directory alias reassigned")
)

type Status string

const (
	Canary   Status = "canary"
	Active   Status = "active"
	Draining Status = "draining"
)

type Node struct {
	Alias        string   `json:"alias"`
	Relay        string   `json:"relay"`
	STUN         []string `json:"stun"`
	Roles        []string `json:"roles"`
	Zone         string   `json:"zone"`
	Region       string   `json:"region"`
	Provider     string   `json:"provider"`
	Operator     string   `json:"operator"`
	CapacityMbps uint32   `json:"capacity_mbps"`
	Status       Status   `json:"status"`
}

func (node Node) HasRole(role string) bool {
	return slices.Contains(node.Roles, role)
}

type STUNEndpoint struct {
	URL           string `json:"url"`
	Region        string `json:"region"`
	Provider      string `json:"provider"`
	FailureDomain string `json:"failure_domain"`
}

type Payload struct {
	Format     int            `json:"format"`
	Network    string         `json:"network"`
	Generation uint64         `json:"generation"`
	Serial     uint64         `json:"serial"`
	IssuedAt   time.Time      `json:"issued_at"`
	ExpiresAt  time.Time      `json:"expires_at"`
	Nodes      []Node         `json:"nodes"`
	PublicSTUN []STUNEndpoint `json:"public_stun"`
}

type Certificate struct {
	Format     int       `json:"format"`
	Network    string    `json:"network"`
	Generation uint64    `json:"generation"`
	Key        string    `json:"key"`
	NotBefore  time.Time `json:"not_before"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type SignedCertificate struct {
	Root      string `json:"root"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type Envelope struct {
	Payload     string            `json:"payload"`
	Certificate SignedCertificate `json:"certificate"`
	Signature   string            `json:"signature"`
}

type Root struct {
	ID  string            `json:"id"`
	Key ed25519.PublicKey `json:"-"`
}

// Snapshot retains verified bytes privately. Callers cannot mutate trusted data.
type Snapshot struct {
	payload     Payload
	certificate Certificate
	digest      [sha256.Size]byte
	envelope    []byte
}

type Cursor struct {
	Generation uint64            `json:"generation"`
	Serial     uint64            `json:"serial"`
	Digest     [sha256.Size]byte `json:"digest"`
}

func (snapshot Snapshot) Cursor() Cursor {
	return Cursor{Generation: snapshot.payload.Generation, Serial: snapshot.payload.Serial, Digest: snapshot.digest}
}

func (snapshot Snapshot) TrustedUntil(grace time.Duration) time.Time {
	expiry := snapshot.payload.ExpiresAt.Add(max(0, min(grace, DefaultGrace)))
	if snapshot.certificate.ExpiresAt.Before(expiry) {
		return snapshot.certificate.ExpiresAt
	}
	return expiry
}

func (snapshot Snapshot) Usable(now time.Time, grace time.Duration) bool {
	return len(snapshot.envelope) > 0 && !now.Add(ClockSkew).Before(snapshot.certificate.NotBefore) &&
		!now.Add(ClockSkew).Before(snapshot.payload.IssuedAt) && now.Before(snapshot.TrustedUntil(grace))
}

func (snapshot Snapshot) Envelope() []byte { return append([]byte(nil), snapshot.envelope...) }
func (snapshot Snapshot) Payload() Payload { return clonePayload(snapshot.payload) }

func clonePayload(payload Payload) Payload {
	payload.Nodes = append([]Node(nil), payload.Nodes...)
	for index := range payload.Nodes {
		payload.Nodes[index].Roles = append([]string(nil), payload.Nodes[index].Roles...)
		payload.Nodes[index].STUN = append([]string(nil), payload.Nodes[index].STUN...)
	}
	payload.PublicSTUN = append([]STUNEndpoint(nil), payload.PublicSTUN...)
	return payload
}

func (snapshot Snapshot) Resolve(alias string) (string, bool) {
	for _, node := range snapshot.payload.Nodes {
		if node.Alias == alias {
			return node.Relay, true
		}
	}
	return "", false
}

func Accept(highest Cursor, next Snapshot, now time.Time, grace time.Duration) error {
	if !next.Usable(now, grace) {
		return ErrExpired
	}
	return Compare(highest, next.Cursor())
}

func Compare(highest, cursor Cursor) error {
	if cursor.Generation < highest.Generation || cursor.Generation == highest.Generation &&
		(cursor.Serial < highest.Serial || cursor.Serial == highest.Serial && cursor.Digest != highest.Digest) {
		return ErrRollback
	}
	return nil
}
