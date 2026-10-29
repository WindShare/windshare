package destinationauthority

import (
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type destinationIdentityProvider interface {
	BindDestinationIdentity(outputcap.Directory) (outputcap.DestinationBindingID, error)
	RetireDestinationIdentity(outputcap.Directory, outputcap.DestinationBindingID) error
}

type directoryIdentityProvider interface {
	CreateRecoveryIdentity() ([]byte, error)
	MatchesRecoveryIdentity([]byte) (bool, error)
	RetireRecoveryIdentity([]byte) error
}

// Retirement follows the durable terminal record, so a crash can retry cleanup
// without ever creating a witness or granting authority over a replacement.
func (reservation *TopLevelReservation) RetireRecoveryIdentity() error {
	if reservation == nil {
		return ErrInvalidReservation
	}
	if reservation.entry.EntryKind() == receivecontract.ContainerEntrySingleFile {
		return nil
	}
	provider, ok := reservation.directory.(directoryIdentityProvider)
	if !ok {
		return ErrInvalidConfiguration
	}
	return provider.RetireRecoveryIdentity(reservation.persistentIdentityClaim)
}

func (authority *BoundDestination) RetireReservationIdentity(expected ExpectedReservation) error {
	if !expected.valid() {
		return ErrInvalidReservation
	}
	entry, err := NewReservedEntry(expected.Reservation)
	if err != nil {
		return err
	}
	return authority.withGuardedRoot(func(root outputcap.Directory) error {
		if expected.Reservation.AuthorityRef() != authority.binding.AuthorityRef() {
			return ErrInvalidReservation
		}
		if entry.EntryKind() == receivecontract.ContainerEntrySingleFile {
			return nil
		}
		directory, err := openExactPublicDirectory(root, entry.PhysicalName())
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		provider, ok := directory.(directoryIdentityProvider)
		if !ok {
			return errors.Join(ErrInvalidConfiguration, directory.Close())
		}
		return errors.Join(provider.RetireRecoveryIdentity(expected.PersistentIdentityClaim), directory.Close())
	})
}

func (authority *BoundDestination) retireDestinationIdentity(control outputcap.Directory) error {
	if !authority.binding.Capabilities().OperationRecovery().Supported() {
		return nil
	}
	provider, ok := authority.platform.(destinationIdentityProvider)
	if !ok {
		return ErrInvalidConfiguration
	}
	return provider.RetireDestinationIdentity(control, authority.binding.ID())
}
