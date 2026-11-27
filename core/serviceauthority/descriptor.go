package serviceauthority

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"time"

	"github.com/windshare/windshare/core/networkendpoint"
)

const (
	descriptorVersion  = 2
	descriptorLifetime = 300
	GossipLifetime     = 24 * 60 * 60
	GossipDomain       = "windshare/node-gossip/v1\x00"
	MaximumSTUNURLs    = 4
	nodeIdentityDomain = "windshare/node-identity/v1\x00"
	nodePlatformNative = "native"
)

type Descriptor struct {
	Version       int    `json:"version"`
	NodeID        string `json:"node_id"`
	NodeKey       string `json:"node_key"`
	RelayEndpoint string `json:"relay_endpoint"`
	Platform      string `json:"platform"`
	Services      struct {
		Relay bool `json:"relay"`
		STUN  bool `json:"stun"`
	} `json:"services"`
	STUN           []string `json:"stun"`
	Zone           string   `json:"zone"`
	Region         string   `json:"region"`
	PublicCapacity *uint64  `json:"public_capacity_bytes_per_second"`
	Operator       string   `json:"operator"`
	Provider       string   `json:"provider"`
	Authority      *struct {
		URL    string `json:"url"`
		Issuer string `json:"issuer"`
	} `json:"authority"`
	Classes          []string    `json:"classes"`
	IssuedAt         uint64      `json:"issued_at"`
	ExpiresAt        uint64      `json:"expires_at"`
	PublicRelay      PublicRelay `json:"public_relay"`
	ConsoleURL       string      `json:"console_url"`
	AccountLoginPage string      `json:"account_login_page,omitempty"`
}
type descriptorEnvelope struct {
	Version    int    `json:"version"`
	Descriptor string `json:"descriptor"`
	Signature  string `json:"signature"`
}

// InspectDescriptor verifies a self-asserted identity for manual enrollment.
// Official admission must still pin that key with VerifyDescriptor.
func InspectDescriptor(data []byte, now time.Time) (Descriptor, error) {
	return inspect(data, now, DescriptorDomain, descriptorLifetime)
}

// GossipDescriptor cannot be supplied to enrollment or official account verification.
type GossipDescriptor struct{ Descriptor }

func InspectGossip(data []byte, now time.Time) (GossipDescriptor, error) {
	d, err := inspect(data, now, GossipDomain, GossipLifetime)
	return GossipDescriptor{d}, err
}

func inspect(data []byte, now time.Time, domain string, lifetime uint64) (Descriptor, error) {
	var envelope descriptorEnvelope
	if len(data) > MaximumDescriptorBytes || json.Unmarshal(data, &envelope) != nil || envelope.Version != descriptorVersion {
		return Descriptor{}, ErrFormat
	}
	payload, err := DecodeBase64(envelope.Descriptor, MaximumDescriptorBytes)
	if err != nil {
		return Descriptor{}, err
	}
	var identity struct {
		Key string `json:"node_key"`
	}
	if json.Unmarshal(payload, &identity) != nil {
		return Descriptor{}, ErrFormat
	}
	key, err := PublicKey(identity.Key)
	if err != nil {
		return Descriptor{}, err
	}
	return verify(data, key, now, domain, lifetime)
}

// RelayBaseURL is for APIs that append their protocol path. The signed descriptor
// carries the complete canonical WebSocket endpoint, never an ambiguous base.
func (descriptor Descriptor) RelayBaseURL() string {
	endpoint, err := url.Parse(descriptor.RelayEndpoint)
	if err != nil {
		return ""
	}
	base, ok := networkendpoint.BaseFromWebSocketURL(*endpoint)
	if !ok {
		return ""
	}
	switch base.Scheme {
	case "ws":
		base.Scheme = "http"
	case "wss":
		base.Scheme = "https"
	default:
		return ""
	}
	return base.String()
}

// OffersCommunityService reports whether this descriptor qualifies for community
// peer discovery: self-hosted (no accounting authority), not opted out, and
// offering either anonymous public relaying or STUN.
func (descriptor Descriptor) OffersCommunityService() bool {
	return descriptor.Authority == nil &&
		descriptor.PublicRelay != RelayDisabled &&
		((descriptor.PublicRelay == RelayAnonymous && descriptor.Services.Relay) || descriptor.Services.STUN)
}

func VerifyDescriptor(data []byte, key [32]byte, now time.Time) (Descriptor, error) {
	return verify(data, key, now, DescriptorDomain, descriptorLifetime)
}

func verify(data []byte, key [32]byte, now time.Time, domain string, lifetime uint64) (Descriptor, error) {
	var envelope descriptorEnvelope
	if len(data) > MaximumDescriptorBytes || json.Unmarshal(data, &envelope) != nil || envelope.Version != descriptorVersion {
		return Descriptor{}, ErrFormat
	}
	payload, err := DecodeBase64(envelope.Descriptor, MaximumDescriptorBytes)
	if err != nil {
		return Descriptor{}, err
	}
	signature, err := DecodeBase64(envelope.Signature, ed25519.SignatureSize)
	if err != nil || !ed25519.Verify(key[:], append([]byte(domain), payload...), signature) {
		return Descriptor{}, ErrSignature
	}
	var descriptor Descriptor
	digest := sha256.Sum256(append([]byte(nodeIdentityDomain), key[:]...))
	if json.Unmarshal(payload, &descriptor) != nil || descriptor.Version != descriptorVersion || descriptor.Platform != nodePlatformNative || descriptor.PublicRelay == RelayUnknown ||
		descriptor.NodeKey != base64.RawURLEncoding.EncodeToString(key[:]) ||
		descriptor.NodeID != base64.RawURLEncoding.EncodeToString(digest[:]) ||
		descriptor.ExpiresAt <= descriptor.IssuedAt || descriptor.ExpiresAt-descriptor.IssuedAt > lifetime {
		return Descriptor{}, ErrFormat
	}
	if descriptor.Authority != nil {
		if _, err := PublicKey(descriptor.Authority.Issuer); err != nil {
			return Descriptor{}, ErrFormat
		}
	}
	if len(descriptor.STUN) > MaximumSTUNURLs || descriptor.Services.STUN && len(descriptor.STUN) == 0 {
		return Descriptor{}, ErrFormat
	}
	endpoint, endpointErr := networkendpoint.NormalizeRelay(descriptor.RelayBaseURL())
	if endpointErr != nil || endpoint.IdentityURL != descriptor.RelayEndpoint ||
		(!descriptor.Services.Relay && (descriptor.PublicRelay == RelayAnonymous || descriptor.PublicRelay == RelayAuthenticated)) {
		return Descriptor{}, ErrFormat
	}
	if now.Unix() < 0 || descriptor.IssuedAt > uint64(now.Unix())+ClockSkew || uint64(now.Unix()) >= descriptor.ExpiresAt {
		return Descriptor{}, ErrExpired
	}
	return descriptor, nil
}
