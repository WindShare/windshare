package incrementaladmission

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
)

func ValidateDirectory(intent transfer.TransferIntent, directory transfer.OutputDirectory) error {
	if directory.DirectoryID.IsZero() || directory.Generation.IsZero() {
		return transfer.ErrInvalidDirectoryAdmission
	}
	if directory.Path == "" {
		if !directory.ParentAdmission.IsZero() || directory.DirectoryID != intent.SyntheticRoot() {
			return transfer.ErrDirectoryAdmissionMismatch
		}
		return nil
	}
	canonical, err := catalog.CanonicalPath(directory.Path)
	if err != nil || canonical != directory.Path || directory.ParentAdmission.IsZero() {
		return transfer.ErrInvalidDirectoryAdmission
	}
	return nil
}

func SameDirectory(left, right transfer.OutputDirectory) bool {
	return left.DirectoryID == right.DirectoryID && left.Generation == right.Generation &&
		left.ParentAdmission.Equal(right.ParentAdmission) && left.Path == right.Path &&
		left.ModifiedTime == right.ModifiedTime
}

// NewSecret centralizes the opaque receipt key policy so every incremental
// adapter rejects deterministic zero authority in exactly the same way.
func NewSecret(random io.Reader) ([sha256.Size]byte, error) {
	var secret [sha256.Size]byte
	if random == nil {
		return secret, errors.New("incremental output admission entropy source is nil")
	}
	if _, err := io.ReadFull(random, secret[:]); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("generate incremental output admission secret: %w", err)
	}
	if secret == ([sha256.Size]byte{}) {
		return [sha256.Size]byte{}, errors.New("incremental output admission secret is zero")
	}
	return secret, nil
}
