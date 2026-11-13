package nodedirectory

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/core/networkendpoint"
)

var encoding = base64.RawURLEncoding.Strict()

// Verify authenticates expired snapshots too, so persisted rollback protection
// survives offline time. Usable/Accept separately decide whether new use is allowed.
func Verify(data []byte, network string, roots []Root) (Snapshot, error) {
	if len(data) == 0 || len(data) > MaximumEnvelopeBytes {
		return Snapshot{}, ErrFormat
	}
	var envelope Envelope
	if err := decode(data, &envelope); err != nil {
		return Snapshot{}, err
	}
	certificateBytes, err := unbase64(envelope.Certificate.Payload, 4096)
	if err != nil {
		return Snapshot{}, err
	}
	var root ed25519.PublicKey
	for _, candidate := range roots {
		if candidate.ID == envelope.Certificate.Root {
			if root != nil {
				return Snapshot{}, ErrSignature
			}
			root = candidate.Key
		}
	}
	if !verify(root, CertificateDomain, certificateBytes, envelope.Certificate.Signature) {
		return Snapshot{}, ErrSignature
	}
	var certificate Certificate
	if err := decode(certificateBytes, &certificate); err != nil {
		return Snapshot{}, err
	}
	if certificate.Format != Format || certificate.Network != network || !validCounter(certificate.Generation) ||
		certificate.NotBefore.IsZero() || !certificate.ExpiresAt.After(certificate.NotBefore) ||
		certificate.ExpiresAt.Sub(certificate.NotBefore) > MaximumCertificateLifetime {
		return Snapshot{}, ErrFormat
	}
	key, err := unbase64(certificate.Key, ed25519.PublicKeySize)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return Snapshot{}, ErrFormat
	}
	payloadBytes, err := unbase64(envelope.Payload, MaximumEnvelopeBytes)
	if err != nil {
		return Snapshot{}, err
	}
	if !verify(key, PayloadDomain, payloadBytes, envelope.Signature) {
		return Snapshot{}, ErrSignature
	}
	var payload Payload
	if err := decode(payloadBytes, &payload); err != nil {
		return Snapshot{}, err
	}
	if payload.Network != network || payload.Generation != certificate.Generation ||
		payload.IssuedAt.Before(certificate.NotBefore) {
		return Snapshot{}, ErrFormat
	}
	if err := Validate(payload); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{payload: payload, certificate: certificate, digest: sha256.Sum256(payloadBytes), envelope: bytes.Clone(data)}, nil
}

func verify(key ed25519.PublicKey, domain string, message []byte, signature string) bool {
	raw, err := unbase64(signature, ed25519.SignatureSize)
	return err == nil && len(key) == ed25519.PublicKeySize && len(raw) == ed25519.SignatureSize &&
		ed25519.Verify(key, append([]byte(domain), message...), raw)
}

func unbase64(raw string, maximum int) ([]byte, error) {
	if len(raw) > encoding.EncodedLen(maximum) {
		return nil, ErrFormat
	}
	value, err := encoding.DecodeString(raw)
	if err != nil || encoding.EncodeToString(value) != raw {
		return nil, ErrFormat
	}
	return value, nil
}

func decode(data []byte, target any) error {
	// Duplicate members have different meanings across JSON implementations.
	// Reject them before decoding signed deployment policy in either language.
	if !utf8.Valid(data) {
		return ErrFormat
	}
	if err := uniqueMembers(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return ErrFormat
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %w", ErrFormat, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ErrFormat
	}
	return nil
}

func uniqueMembers(decoder *json.Decoder) error {
	var visit func(int) error
	visit = func(depth int) error {
		if depth > 16 {
			return ErrFormat
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			return visitMembers(decoder, depth, visit)
		case '[':
			for decoder.More() {
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		default:
			return ErrFormat
		}
		_, err = decoder.Token()
		return err
	}
	if err := visit(0); err != nil {
		return err
	}
	_, err := decoder.Token()
	if err != io.EOF {
		return ErrFormat
	}
	return nil
}

func visitMembers(decoder *json.Decoder, depth int, visit func(int) error) error {
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || seen[name] || !validMemberName(name) {
			return ErrFormat
		}
		seen[name] = true
		if err := visit(depth + 1); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
}

func validCounter(value uint64) bool { return value > 0 && value <= MaximumCounter }

func validMemberName(name string) bool {
	// encoding/json matches struct fields case-insensitively. The wire schema
	// uses lowercase ASCII names, matching the browser's exact-key validation.
	for _, char := range name {
		if (char < 'a' || char > 'z') && char != '_' {
			return false
		}
	}
	return name != ""
}

func validText(value string) bool {
	return value != "" && len(value) <= 128 && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "\x00\r\n\t")
}

func ValidAlias(alias string) bool {
	if len(alias) == 0 || len(alias) > 32 || alias[0] < 'a' || alias[0] > 'z' {
		return false
	}
	for _, char := range alias {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return !strings.HasSuffix(alias, "-")
}

func Validate(payload Payload) error {
	if payload.Format != Format || !validText(payload.Network) || !validCounter(payload.Generation) || !validCounter(payload.Serial) ||
		payload.IssuedAt.IsZero() || !payload.ExpiresAt.After(payload.IssuedAt) || payload.ExpiresAt.Sub(payload.IssuedAt) > MaximumPayloadLifetime ||
		len(payload.Nodes) > MaximumNodes || len(payload.PublicSTUN) > MaximumPublicSTUN {
		return ErrFormat
	}
	aliases, endpoints := map[string]bool{}, map[[sha256.Size]byte]bool{}
	stunURLs := map[string]bool{}
	for _, node := range payload.Nodes {
		endpoint, err := networkendpoint.NormalizeRelay(node.Relay)
		if err != nil || !strings.HasPrefix(node.Relay, "https://") || strings.ContainsAny(node.Relay, "?#") ||
			!ValidAlias(node.Alias) || aliases[node.Alias] || endpoints[endpoint.Identity] ||
			(node.Zone != "cn" && node.Zone != "global") || !validText(node.Region) || !validText(node.Provider) || !validText(node.Operator) ||
			node.CapacityMbps == 0 || (node.Status != Active && node.Status != Canary && node.Status != Draining) || len(node.STUN) > 4 {
			return ErrFormat
		}
		aliases[node.Alias], endpoints[endpoint.Identity] = true, true
		if !validRoles(node) {
			return ErrFormat
		}
		for _, raw := range node.STUN {
			if err := addSTUN(stunURLs, raw); err != nil {
				return err
			}
		}
	}
	for _, endpoint := range payload.PublicSTUN {
		if !validText(endpoint.Region) || !validText(endpoint.Provider) || !validText(endpoint.FailureDomain) {
			return ErrFormat
		}
		if err := addSTUN(stunURLs, endpoint.URL); err != nil {
			return err
		}
	}
	return nil
}

func validRoles(node Node) bool {
	roles := map[string]bool{}
	for _, role := range node.Roles {
		if (role != "relay" && role != "stun") || roles[role] {
			return false
		}
		roles[role] = true
	}
	return len(roles) > 0 && roles["stun"] == (len(node.STUN) > 0)
}

func addSTUN(seen map[string]bool, raw string) error {
	normalized, err := networkendpoint.NormalizeSTUN(raw)
	if err != nil || normalized != raw || seen[normalized] {
		return ErrFormat
	}
	seen[normalized] = true
	return nil
}
