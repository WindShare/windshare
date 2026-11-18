package relaysetup

import (
	"errors"

	"github.com/windshare/windshare/core/serviceauthority"
)

const nativeKeyAccess = "local-key"

var ErrLocalCredentialRequired = errors.New("this node requires an access key from its owner")

// Native nodes attest their own identity. Cloudflare private relays separately
// advertise browser device enrollment; a native key never implies that workflow.
func (run *workflow) discoverNative(raw []byte, address string) (discovery, error) {
	descriptor, err := serviceauthority.InspectDescriptor(raw, run.dependencies.Now())
	if err != nil {
		return discovery{}, err
	}
	if !sameRelay(address, descriptor.RelayBaseURL()) {
		return discovery{}, errors.New("node descriptor does not match the requested relay")
	}
	access := nativeKeyAccess
	if descriptor.Anonymous {
		access = "open"
	}
	return discovery{Version: 2, Access: access, RelayURL: descriptor.RelayBaseURL()}, nil
}
