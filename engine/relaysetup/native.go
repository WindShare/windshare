package relaysetup

import (
	"errors"

	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/relayprofile"
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
	if descriptor.PublicRelay == serviceauthority.RelayAnonymous {
		access = "open"
	}
	result := discovery{Version: 2, Access: access, RelayURL: descriptor.RelayBaseURL(), ConsoleURL: descriptor.ConsoleURL}
	result.Node = relayprofile.NodeIdentity{Platform: descriptor.Platform, NodeKey: descriptor.NodeKey}
	result.PublicRelay = descriptor.PublicRelay
	if descriptor.Authority != nil {
		result.Node.Authority = descriptor.Authority.Issuer
	}
	if descriptor.Services.Relay {
		result.AccountLoginPage = descriptor.AccountLoginPage
	}
	return result, nil
}
