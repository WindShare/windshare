package serviceauthority

import "time"

type Generation struct {
	Issuer    [32]byte
	Number    uint64
	Key       [32]byte
	NotBefore uint64
	ExpiresAt uint64
}
type certificateWire struct {
	_         struct{} `cbor:",toarray"`
	Format    uint64
	Issuer    []byte
	Number    uint64
	Key       []byte
	NotBefore uint64
	ExpiresAt uint64
}

func VerifyGeneration(certificate string, root [32]byte) (Generation, error) {
	payload, err := VerifySigned(CertificateDomain, certificate, root, MaximumTicketBytes)
	if err != nil {
		return Generation{}, err
	}
	var wire certificateWire
	if decodeCanonical(payload, &wire) != nil || wire.Format != 1 ||
		len(wire.Issuer) != 32 || len(wire.Key) != 32 || wire.Number == 0 || wire.Number > MaximumCounter ||
		wire.ExpiresAt <= wire.NotBefore || wire.ExpiresAt-wire.NotBefore > CertificateLifetime {
		return Generation{}, ErrFormat
	}
	generation := Generation{Issuer: [32]byte(wire.Issuer), Number: wire.Number, Key: [32]byte(wire.Key),
		NotBefore: wire.NotBefore, ExpiresAt: wire.ExpiresAt}
	if root == [32]byte{} || generation.Issuer != root {
		return Generation{}, ErrSignature
	}
	return generation, nil
}

func (generation Generation) Usable(now time.Time) bool {
	return now.Unix() >= 0 && uint64(now.Unix())+ClockSkew >= generation.NotBefore &&
		uint64(now.Unix()) < generation.ExpiresAt && generation.Number > 0
}

// Compare is independent of wall time: expiry never erases the rollback barrier.
func Compare(previous, next Generation) error {
	if previous.Number > next.Number || previous.Number == next.Number && previous != next {
		return ErrRollback
	}
	return nil
}
