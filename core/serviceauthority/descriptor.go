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

const descriptorLifetime = 300
const nodeIdentityDomain = "windshare/node-identity/v1\x00"

type Descriptor struct {
	Version       int    `json:"version"`
	NodeID        string `json:"node_id"`
	NodeKey       string `json:"node_key"`
	RelayEndpoint string `json:"relay_endpoint"`
	Services      struct {
		Relay bool `json:"relay"`
		STUN  bool `json:"stun"`
	} `json:"services"`
	STUN             []string `json:"stun"`
	Zone             string   `json:"zone"`
	Region           string   `json:"region"`
	Capacity         *uint64  `json:"capacity_bytes_per_second"`
	Classes          []string `json:"classes"`
	IssuedAt         uint64   `json:"issued_at"`
	ExpiresAt        uint64   `json:"expires_at"`
	Available        bool     `json:"relay_available"`
	Anonymous        bool     `json:"anonymous"`
	ConsoleURL       string   `json:"console_url"`
	AccountLoginPage string   `json:"account_login_page,omitempty"`
}
type descriptorEnvelope struct {
	Version    int    `json:"version"`
	Descriptor string `json:"descriptor"`
	Signature  string `json:"signature"`
}

// InspectDescriptor verifies a self-asserted identity for manual enrollment.
// Official admission must still pin that key with VerifyDescriptor.
func InspectDescriptor(data []byte, now time.Time) (Descriptor, error) {
	var envelope descriptorEnvelope
	if len(data) > MaximumDescriptorBytes || json.Unmarshal(data, &envelope) != nil || envelope.Version != 2 {
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
	return VerifyDescriptor(data, key, now)
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

func VerifyDescriptor(data []byte, key [32]byte, now time.Time) (Descriptor, error) {
	var envelope descriptorEnvelope
	if len(data) > MaximumDescriptorBytes || json.Unmarshal(data, &envelope) != nil || envelope.Version != 2 {
		return Descriptor{}, ErrFormat
	}
	payload, err := DecodeBase64(envelope.Descriptor, MaximumDescriptorBytes)
	if err != nil {
		return Descriptor{}, err
	}
	signature, err := DecodeBase64(envelope.Signature, ed25519.SignatureSize)
	if err != nil || !ed25519.Verify(key[:], append([]byte(DescriptorDomain), payload...), signature) {
		return Descriptor{}, ErrSignature
	}
	var descriptor Descriptor
	digest := sha256.Sum256(append([]byte(nodeIdentityDomain), key[:]...))
	if json.Unmarshal(payload, &descriptor) != nil || descriptor.Version != 2 ||
		descriptor.NodeKey != base64.RawURLEncoding.EncodeToString(key[:]) ||
		descriptor.NodeID != base64.RawURLEncoding.EncodeToString(digest[:]) ||
		descriptor.ExpiresAt <= descriptor.IssuedAt || descriptor.ExpiresAt-descriptor.IssuedAt > descriptorLifetime {
		return Descriptor{}, ErrFormat
	}
	endpoint, endpointErr := networkendpoint.NormalizeRelay(descriptor.RelayBaseURL())
	if endpointErr != nil || endpoint.IdentityURL != descriptor.RelayEndpoint {
		return Descriptor{}, ErrFormat
	}
	if now.Unix() < 0 || descriptor.IssuedAt > uint64(now.Unix())+ClockSkew || uint64(now.Unix()) >= descriptor.ExpiresAt {
		return Descriptor{}, ErrExpired
	}
	return descriptor, nil
}
