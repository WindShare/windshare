package pionpeer

import (
	pion "github.com/pion/webrtc/v4"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

// TURN uses the system route to the allocation server. Direct interface
// filters, gateway maps and leased host sockets do not apply to this transport.
func NewTURNPeerConnection(base pion.Configuration, relay iceconfig.TURNRelay, identity AttemptConfig) (*Connection, error) {
	urls, user, password := relay.Credentials()
	if len(urls) == 0 {
		return nil, iceconfig.ErrConfiguration
	}
	base.ICEServers = []pion.ICEServer{{URLs: urls, Username: user, Credential: password, CredentialType: pion.ICECredentialTypePassword}}
	base.ICETransportPolicy = pion.ICETransportPolicyRelay
	pc, err := pion.NewAPI().NewPeerConnection(base)
	if err != nil {
		return nil, err
	}
	c := &Connection{PeerConnection: pc, request: identity, release: func() {}}
	c.OnICEConnectionStateChange(nil)
	c.OnConnectionStateChange(nil)
	pc.SCTP().Transport().ICETransport().OnSelectedCandidatePairChange(c.observeSelectedPair)
	c.observe("provider_created", "turn")
	return c, nil
}
