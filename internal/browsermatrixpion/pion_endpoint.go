package browsermatrixpion

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
)

// PionEndpointConfig describes the operator-owned public endpoint used by the
// remote browser-matrix service. It is intentionally separate from attempt
// construction so correctness tests can inject a socket-owning implementation.
type PionEndpointConfig struct {
	PublicIP   string
	UDPPortMin uint16
	UDPPortMax uint16
}

func NewPionEndpointAPI(config PionEndpointConfig) (*pion.API, error) {
	address, err := netip.ParseAddr(config.PublicIP)
	if err != nil || !address.Is4() || address.String() != config.PublicIP ||
		config.UDPPortMin == 0 || config.UDPPortMax < config.UDPPortMin {
		return nil, errors.New("remote Pion endpoint authority is invalid")
	}
	var setting pion.SettingEngine
	setting.SetNetworkTypes([]pion.NetworkType{pion.NetworkTypeUDP4})
	if err := setting.SetEphemeralUDPPortRange(config.UDPPortMin, config.UDPPortMax); err != nil {
		return nil, fmt.Errorf("bind remote Pion UDP authority: %w", err)
	}
	// The configured address is operator-authorized public routing state. Pion
	// must advertise it instead of an incidental interface discovered at runtime.
	if err := setting.SetICEAddressRewriteRules(pion.ICEAddressRewriteRule{
		External:        []string{config.PublicIP},
		AsCandidateType: pion.ICECandidateTypeHost,
		Mode:            pion.ICEAddressRewriteReplace,
	}); err != nil {
		return nil, fmt.Errorf("configure remote Pion address rewrite: %w", err)
	}
	setting.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return pion.NewAPI(pion.WithSettingEngine(setting)), nil
}
