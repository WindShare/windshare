// Package senderauth defines the sender-signature acceptance policy shared by
// native and browser receivers, independently of their cryptographic backends.
package senderauth

import (
	"bytes"
	"crypto/ed25519"
	"errors"

	"filippo.io/edwards25519"
)

var ErrPublicKey = errors.New("senderauth: public key must be canonical, nonzero and prime-order")

// Verifier owns a checked key snapshot and is safe for concurrent verification.
type Verifier struct {
	publicKey ed25519.PublicKey
}

// NewVerifier rejects the exceptional key encodings on which Ed25519 backends
// disagree. Prime-order identities also make the portable cofactored equation
// equivalent to the native equation when the signature point is prime-order.
func NewVerifier(publicKey ed25519.PublicKey) (*Verifier, error) {
	point, err := canonicalPoint(publicKey)
	if err != nil || smallOrder(point) {
		return nil, ErrPublicKey
	}
	// ScalarMult reduces scalars modulo L, so test [L-1]A + A instead of [L]A.
	orderMinusOne := []byte{
		0xec, 0xd3, 0xf5, 0x5c, 0x1a, 0x63, 0x12, 0x58,
		0xd6, 0x9c, 0xf7, 0xa2, 0xde, 0xf9, 0xde, 0x14,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x10,
	}
	scalar, _ := new(edwards25519.Scalar).SetCanonicalBytes(orderMinusOne)
	product := new(edwards25519.Point).ScalarMult(scalar, point)
	if product.Add(product, point).Equal(edwards25519.NewIdentityPoint()) != 1 {
		return nil, ErrPublicKey
	}
	return &Verifier{publicKey: bytes.Clone(publicKey)}, nil
}

func (v *Verifier) Verify(message, signature []byte) bool {
	if v == nil || len(v.publicKey) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize {
		return false
	}
	point, err := canonicalPoint(signature[:ed25519.PublicKeySize])
	if err != nil || smallOrder(point) {
		return false
	}
	// The standard library checks canonical S and the cofactorless equation.
	// With a prime-order A, a successful equation implies prime-order R.
	return ed25519.Verify(v.publicKey, message, signature)
}

// Verify is the stateless boundary for codecs that do not own a sender lifetime.
func Verify(publicKey ed25519.PublicKey, message, signature []byte) bool {
	verifier, err := NewVerifier(publicKey)
	return err == nil && verifier.Verify(message, signature)
}

func canonicalPoint(encoded []byte) (*edwards25519.Point, error) {
	point, err := new(edwards25519.Point).SetBytes(encoded)
	if err != nil || !bytes.Equal(point.Bytes(), encoded) {
		return nil, ErrPublicKey
	}
	return point, nil
}

func smallOrder(point *edwards25519.Point) bool {
	return new(edwards25519.Point).MultByCofactor(point).Equal(edwards25519.NewIdentityPoint()) == 1
}
