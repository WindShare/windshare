package testicetopology

import (
	"fmt"
	"net"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
)

// PionConfiguration maps the serialized RTC policy without inheriting Pion's
// or WindShare production's public-STUN defaults.
func PionConfiguration(profile Profile) (pion.Configuration, error) {
	if err := profile.Validate(); err != nil {
		return pion.Configuration{}, err
	}
	return pion.Configuration{
		ICEServers:         []pion.ICEServer{},
		ICETransportPolicy: pion.ICETransportPolicyAll,
	}, nil
}

// PionSettingEngine binds candidate gathering to the runtime resolution that
// the evidence verdict later audits. The local address is deliberately narrower
// than the eligible remote inventory: the frozen route probe names one source
// address as Pion's authoritative endpoint.
func PionSettingEngine(profile Profile, resolution Resolution) (pion.SettingEngine, error) {
	profileSHA256, err := profile.SHA256()
	if err != nil {
		return pion.SettingEngine{}, err
	}
	if err := resolution.Validate(profile, profileSHA256); err != nil {
		return pion.SettingEngine{}, err
	}
	policy, err := newPionNetworkPolicy(resolution)
	if err != nil {
		return pion.SettingEngine{}, err
	}
	var setting pion.SettingEngine
	setting.SetNetworkTypes([]pion.NetworkType{pion.NetworkTypeUDP4})
	setting.SetInterfaceFilter(policy.allowsInterface)
	setting.SetIPFilter(policy.allowsLocalIP)
	setting.SetRemoteIPFilter(policy.allowsRemoteIP)
	// Browsers may protect host addresses behind .local candidates. Query-only
	// keeps Pion's own candidate numeric while retaining browser interoperability.
	setting.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryOnly)
	return setting, nil
}

type pionNetworkPolicy struct {
	interfaceName   string
	localAddress    string
	remoteAddresses map[string]struct{}
}

func newPionNetworkPolicy(resolution Resolution) (pionNetworkPolicy, error) {
	if resolution.Interface.Name == "" || !IsOperationalIPv4Unicast(resolution.Interface.SelectedAddress) {
		return pionNetworkPolicy{}, fmt.Errorf("%w: Pion endpoint authority is incomplete", ErrInvalidResolution)
	}
	remote := make(map[string]struct{}, len(resolution.Interface.EligibleAddresses))
	for _, candidate := range resolution.Interface.EligibleAddresses {
		if !IsOperationalIPv4Unicast(candidate.Address) {
			return pionNetworkPolicy{}, fmt.Errorf("%w: Pion remote address inventory is invalid", ErrInvalidResolution)
		}
		remote[candidate.Address] = struct{}{}
	}
	if len(remote) == 0 {
		return pionNetworkPolicy{}, fmt.Errorf("%w: Pion remote address inventory is empty", ErrInvalidResolution)
	}
	return pionNetworkPolicy{
		interfaceName: resolution.Interface.Name, localAddress: resolution.Interface.SelectedAddress,
		remoteAddresses: remote,
	}, nil
}

func (policy pionNetworkPolicy) allowsInterface(name string) bool {
	return name == policy.interfaceName
}

func (policy pionNetworkPolicy) allowsLocalIP(address net.IP) bool {
	return canonicalIPv4(address) == policy.localAddress
}

func (policy pionNetworkPolicy) allowsRemoteIP(address net.IP) bool {
	_, allowed := policy.remoteAddresses[canonicalIPv4(address)]
	return allowed
}

func canonicalIPv4(address net.IP) string {
	if address == nil || address.To4() == nil {
		return ""
	}
	return address.To4().String()
}
