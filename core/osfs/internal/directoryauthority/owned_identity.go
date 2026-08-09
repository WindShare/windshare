package directoryauthority

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

const ownedDirectoryIdentityDomain = "windshare/native-owned-directory/v1"

// OwnedDirectoryID binds a durable admission to the native object retained by
// the authority. Reopening through openGuardedDirectory revalidates the entire
// ancestry first, so the resulting opaque ID cannot accidentally certify a
// path-replaced directory.
func (authority *Authority) OwnedDirectoryID(claim outputsession.DirectoryClaim) (transfer.OwnedObjectID, error) {
	if authority == nil || claim.ID() == 0 || claim.Admission().IsZero() {
		return transfer.OwnedObjectID{}, ErrInvalidClaim
	}
	authority.gate.RLock()
	defer authority.gate.RUnlock()
	directory, cleanup, err := authority.openGuardedDirectory(claim.ID())
	if err != nil {
		return transfer.OwnedObjectID{}, err
	}
	owned, identityErr := PersistentOwnedDirectoryID(directory)
	closeErr := cleanup()
	if identityErr != nil || closeErr != nil {
		return transfer.OwnedObjectID{}, errors.Join(identityErr, closeErr)
	}
	return owned, nil
}

// PersistentOwnedDirectoryID enrolls a live, already-guarded directory and
// projects its durable claim into the identity stored by admission records.
func PersistentOwnedDirectoryID(directory outputcap.Directory) (transfer.OwnedObjectID, error) {
	if directory == nil {
		return transfer.OwnedObjectID{}, ErrRetainedAuthorityChanged
	}
	claimBytes, supported, identityErr := persistentDirectoryIdentityClaim(directory)
	if !supported {
		return transfer.OwnedObjectID{}, outputcap.ErrRecoverableOutputUnsupported
	}
	if identityErr != nil || len(claimBytes) == 0 || len(claimBytes) > outputcap.MaxRootIdentityClaimBytes {
		return transfer.OwnedObjectID{}, errors.Join(ErrRetainedAuthorityChanged, identityErr)
	}
	hash := sha256.New()
	writeOwnedDirectoryIdentityField(hash, []byte(ownedDirectoryIdentityDomain))
	writeOwnedDirectoryIdentityField(hash, claimBytes)
	return transfer.OwnedObjectIDFromBytes(hash.Sum(nil))
}

func persistentDirectoryIdentityClaim(directory outputcap.Directory) ([]byte, bool, error) {
	if preparer, ok := directory.(outputcap.PersistentDirectoryIdentityPreparer); ok {
		claim, err := preparer.PreparePersistentDirectoryIdentityClaim()
		return claim, true, err
	}
	identity, ok := directory.(outputcap.PersistentDirectoryIdentity)
	if !ok {
		return nil, false, nil
	}
	claim, err := identity.PersistentDirectoryIdentityClaim()
	return claim, true, err
}

func writeOwnedDirectoryIdentityField(destination interface{ Write([]byte) (int, error) }, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = destination.Write(length[:])
	_, _ = destination.Write(value)
}
