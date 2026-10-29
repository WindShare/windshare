package outputportable

import (
	"crypto/sha256"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

const destinationIdentityName = "destination.identity"

func portableDestinationID(claim []byte) (outputcap.DestinationBindingID, error) {
	if _, err := directoryIdentityName(claim); err != nil {
		return outputcap.DestinationBindingID{}, err
	}
	digest := sha256.Sum256(append([]byte("windshare/portable-destination/v1\x00"), claim...))
	return outputcap.DestinationBindingIDFromBytes(digest[:])
}

// The private namespace owns the destination's restart identity. Native handles
// still guard live placement; directory timestamps never identify retained tasks.
func (*Platform) BindDestinationIdentity(control outputcap.Directory) (outputcap.DestinationBindingID, error) {
	d, ok := control.(*directory)
	if !ok {
		return outputcap.DestinationBindingID{}, outputcap.ErrUnsafeNamespace
	}
	file, err := d.OpenObservedFile(destinationIdentityName, true)
	if errors.Is(err, fs.ErrNotExist) {
		candidate, claim, createErr := d.createDirectoryIdentity()
		if createErr != nil {
			return outputcap.DestinationBindingID{}, createErr
		}
		name, _ := directoryIdentityName(claim)
		outcome, publishErr := d.PublishFileNoReplace(candidate, destinationIdentityName)
		removeErr := d.RemoveFile(name, candidate)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
		closeErr := candidate.Close()
		if publishErr != nil || removeErr != nil || closeErr != nil ||
			(outcome != outputcap.PublishNoReplaceCommitted && outcome != outputcap.PublishNoReplaceCollision) {
			return outputcap.DestinationBindingID{}, errors.Join(outputcap.ErrUnsafeNamespace, publishErr, removeErr, closeErr)
		}
		if err := d.Sync(); err != nil {
			return outputcap.DestinationBindingID{}, err
		}
		file, err = d.OpenObservedFile(destinationIdentityName, true)
	}
	if err != nil {
		return outputcap.DestinationBindingID{}, err
	}
	claim, readErr := readDirectoryIdentity(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return outputcap.DestinationBindingID{}, errors.Join(readErr, closeErr)
	}
	return portableDestinationID(claim)
}

func (*Platform) RetireDestinationIdentity(control outputcap.Directory, expected outputcap.DestinationBindingID) error {
	d, ok := control.(*directory)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	file, err := d.OpenObservedFile(destinationIdentityName, true)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	claim, readErr := readDirectoryIdentity(file)
	id, idErr := portableDestinationID(claim)
	if readErr != nil || idErr != nil || id != expected {
		return errors.Join(outputcap.ErrUnsafeNamespace, readErr, idErr, file.Close())
	}
	removeErr := d.RemoveFile(destinationIdentityName, file)
	if removeErr == nil {
		removeErr = d.Sync()
	}
	return errors.Join(removeErr, file.Close())
}
