package outputcap

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
)

const (
	OutputRootBindingBytes    = sha256.Size
	MaxRootIdentityClaimBytes = 256
)

var ErrInvalidRootBinding = errors.New("osfs: output root binding is invalid")

type CertificationID = checkpointmodel.CertificationID

const (
	CertificationLinuxExt4ProcessRestart   = checkpointmodel.CertificationLinuxExt4ProcessRestart
	CertificationWindowsNTFSProcessRestart = checkpointmodel.CertificationWindowsNTFSProcessRestart
)

func NewCertificationID(value string) (CertificationID, error) {
	return checkpointmodel.NewCertificationID(value)
}

// OutputRootBinding commits to both the certified volume and the current root
// object. Keeping the opaque claims out of durable state prevents a copied
// native identifier from becoming filesystem authority after restart.
type OutputRootBinding struct {
	certification CertificationID
	digest        [OutputRootBindingBytes]byte
}

func NewOutputRootBinding(
	certification CertificationID,
	volumeIdentity []byte,
	objectIdentity []byte,
) (OutputRootBinding, error) {
	validated, err := NewCertificationID(string(certification))
	if err != nil || len(volumeIdentity) == 0 || len(volumeIdentity) > MaxRootIdentityClaimBytes ||
		len(objectIdentity) == 0 || len(objectIdentity) > MaxRootIdentityClaimBytes {
		return OutputRootBinding{}, fmt.Errorf("%w: native identity claims", ErrInvalidRootBinding)
	}
	hash := sha256.New()
	writeRootBindingClaim(hash, []byte("windshare/output-root-binding/v1"))
	writeRootBindingClaim(hash, []byte(validated))
	writeRootBindingClaim(hash, volumeIdentity)
	writeRootBindingClaim(hash, objectIdentity)
	var digest [OutputRootBindingBytes]byte
	copy(digest[:], hash.Sum(nil))
	if digest == ([OutputRootBindingBytes]byte{}) {
		return OutputRootBinding{}, fmt.Errorf("%w: zero digest", ErrInvalidRootBinding)
	}
	return OutputRootBinding{certification: validated, digest: digest}, nil
}

func writeRootBindingClaim(destination io.Writer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = destination.Write(length[:])
	_, _ = destination.Write(value)
}

func (binding OutputRootBinding) Certification() CertificationID { return binding.certification }

func (binding OutputRootBinding) Bytes() []byte {
	return append([]byte(nil), binding.digest[:]...)
}

func (binding OutputRootBinding) String() string {
	return hex.EncodeToString(binding.digest[:])
}

func (binding OutputRootBinding) IsZero() bool {
	return binding.certification == "" && binding.digest == ([OutputRootBindingBytes]byte{})
}
