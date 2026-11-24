// Package serviceauthority verifies account authority records without I/O.
package serviceauthority

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"

	"github.com/fxamacker/cbor/v2"
)

const (
	CertificateDomain      = "windshare/authority-certificate/v1\x00"
	TicketDomain           = "windshare/account-ticket/v1\x00"
	DescriptorDomain       = "windshare/node-descriptor/v2\x00"
	MaximumTicketBytes     = 512
	MaximumDescriptorBytes = 16 << 10
	MaximumResponseBytes   = 512 << 10
	MaximumCounter         = 1<<53 - 1
	DaySeconds             = 86400
	ClockSkew              = 300
	TicketLifetime         = 12 * 60 * 60
	EntitlementLifetime    = 5 * 60
	CertificateLifetime    = 180 * DaySeconds
)

var (
	ErrFormat    = errors.New("invalid authority record")
	ErrSignature = errors.New("authority signature rejected")
	ErrExpired   = errors.New("authority record expired")
	ErrRollback  = errors.New("authority signing generation rollback or equivocation")
)

var encoder = func() cbor.EncMode {
	mode, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	return mode
}()
var decoder = func() cbor.DecMode {
	mode, err := (cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF,
		IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden,
		MaxNestedLevels: 4, MaxArrayElements: 16, MaxMapPairs: 16}).DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

func decodeCanonical(data []byte, target any) error {
	if err := decoder.Unmarshal(data, target); err != nil {
		return ErrFormat
	}
	canonical, err := encoder.Marshal(target)
	if err != nil || !bytes.Equal(data, canonical) {
		return ErrFormat
	}
	return nil
}

func DecodeBase64(value string, limit int) ([]byte, error) {
	if len(value) > base64.RawURLEncoding.EncodedLen(limit) {
		return nil, ErrFormat
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(data) > limit || base64.RawURLEncoding.EncodeToString(data) != value {
		return nil, ErrFormat
	}
	return data, nil
}

func PublicKey(value string) ([32]byte, error) {
	data, err := DecodeBase64(value, ed25519.PublicKeySize)
	if err != nil || len(data) != ed25519.PublicKeySize {
		return [32]byte{}, ErrFormat
	}
	key := [32]byte(data)
	if key == [32]byte{} {
		return key, ErrFormat
	}
	return key, nil
}

func VerifySigned(domain, encoded string, key [32]byte, limit int) ([]byte, error) {
	signed, err := DecodeBase64(encoded, limit)
	if err != nil || len(signed) < ed25519.SignatureSize {
		return nil, ErrFormat
	}
	payload := signed[:len(signed)-ed25519.SignatureSize]
	if !ed25519.Verify(ed25519.PublicKey(key[:]), append([]byte(domain), payload...), signed[len(payload):]) {
		return nil, ErrSignature
	}
	return payload, nil
}
