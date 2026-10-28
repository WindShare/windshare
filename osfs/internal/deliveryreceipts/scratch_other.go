//go:build !windows

package deliveryreceipts

import (
	"errors"
	"os"
)

func newScratchFile() (scratchFile, error) {
	file, err := os.CreateTemp("", "windshare-receipts-*")
	if err != nil {
		return nil, err
	}
	// Only this live handle can read the history. Process termination also
	// reclaims storage, without creating discoverable recovery candidates.
	if err := os.Remove(file.Name()); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}
