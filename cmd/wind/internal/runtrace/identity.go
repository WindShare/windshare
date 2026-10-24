package runtrace

import (
	"encoding/hex"
	"io"

	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
)

type runIdentity [appobs.IdentityBytes]byte

func newRunID(random io.Reader) (runIdentity, error) {
	var identity runIdentity
	if _, err := io.ReadFull(random, identity[:]); err != nil {
		return runIdentity{}, err
	}
	if !identity.valid() {
		return runIdentity{}, ErrRunIDUnavailable
	}
	return identity, nil
}

func (identity runIdentity) valid() bool {
	return identity != (runIdentity{})
}

func (identity runIdentity) encoded() string {
	return wire.EncodeIdentity(identity[:])
}

// Filename correlation is deliberately shorter and visually filesystem-safe;
// the full local run identity remains available only inside each v4 record.
func (identity runIdentity) filenameToken() string {
	return hex.EncodeToString(identity[:])[:directoryFilenameTokenHexLength]
}
