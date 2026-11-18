package relaysetup

import (
	"errors"

	"github.com/windshare/windshare/core/serviceauthority"
)

const nativeKeyAccess = "local-key"

var ErrLocalCredentialRequired = errors.New("this node requires an access key from its owner")

// Only the signed payload can advertise pairing for a native node.
func (run *workflow) discoverNative(raw []byte) (discovery, error) {
	descriptor, err := serviceauthority.InspectDescriptor(raw, run.dependencies.Now())
	if err != nil {
		return discovery{}, err
	}
	access := nativeKeyAccess
	if descriptor.Anonymous {
		access = "open"
	}
	return discovery{Version: 2, Access: access, RelayURL: descriptor.RelayBaseURL(), ConsoleURL: descriptor.ConsoleURL}, nil
}
