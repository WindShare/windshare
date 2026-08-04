package testloopback

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
)

// PionAPI owns one held UDP mux and every PeerConnection created through it.
// Separate logical endpoints should use separate PionAPI values so their ICE
// candidates describe distinct, continuously-held sockets.
type PionAPI struct {
	api    *pion.API
	socket *UDPConn
	mux    *ice.UDPMuxDefault

	mu        sync.Mutex
	peers     []*pion.PeerConnection
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

func (fixture *Fixture) NewPionAPI() *PionAPI {
	fixture.t.Helper()
	api, err := newPionAPI()
	if err != nil {
		fixture.t.Fatalf("create loopback-only Pion API: %v", err)
	}
	if err := fixture.own("Pion endpoint "+api.LocalAddr().String(), api); err != nil {
		_ = api.Close()
		fixture.t.Fatalf("own loopback-only Pion API: %v", err)
	}
	return api
}

func newPionAPI() (*PionAPI, error) {
	interfaces, err := loopbackInterfaces()
	if err != nil {
		return nil, err
	}
	socket, err := listenUDP()
	if err != nil {
		return nil, err
	}
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: socket})
	var setting pion.SettingEngine
	setting.SetNetworkTypes([]pion.NetworkType{pion.NetworkTypeUDP4})
	setting.SetIncludeLoopbackCandidate(true)
	setting.SetInterfaceFilter(func(name string) bool {
		_, allowed := interfaces[name]
		return allowed
	})
	setting.SetIPFilter(isExactLoopbackIPv4)
	setting.SetRemoteIPFilter(isExactLoopbackIPv4)
	// Numeric candidates make the local and remote address policy observable;
	// mDNS discovery would introduce a second host-resolution authority.
	setting.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	setting.SetICEUDPMux(mux)
	return &PionAPI{
		api: pion.NewAPI(pion.WithSettingEngine(setting)), socket: socket, mux: mux,
	}, nil
}

func (api *PionAPI) NewPeerConnection(
	configuration pion.Configuration,
) (*pion.PeerConnection, error) {
	if api == nil {
		return nil, ErrClosed
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.closed {
		return nil, ErrClosed
	}
	peer, err := api.api.NewPeerConnection(configuration)
	if err != nil {
		return nil, err
	}
	api.peers = append(api.peers, peer)
	return peer, nil
}

func (api *PionAPI) LocalAddr() *net.UDPAddr {
	if api == nil || api.socket == nil {
		return nil
	}
	address, _ := api.socket.LocalAddr().(*net.UDPAddr)
	if address == nil {
		return nil
	}
	return &net.UDPAddr{IP: append(net.IP(nil), address.IP...), Port: address.Port, Zone: address.Zone}
}

func (api *PionAPI) Close() error {
	if api == nil {
		return nil
	}
	api.closeOnce.Do(func() {
		api.mu.Lock()
		api.closed = true
		peers := append([]*pion.PeerConnection(nil), api.peers...)
		api.peers = nil
		api.mu.Unlock()

		var failures []error
		for index := range slices.Backward(peers) {
			if err := peers[index].Close(); err != nil {
				failures = append(failures, fmt.Errorf("close Pion peer %d: %w", index, err))
			}
		}
		if err := api.mux.Close(); err != nil {
			failures = append(failures, fmt.Errorf("close Pion UDP mux: %w", err))
		}
		// UDPMuxDefault intentionally suppresses its PacketConn close error. The
		// idempotent owned wrapper retains it so cleanup cannot silently pass.
		if err := api.socket.Close(); err != nil {
			failures = append(failures, fmt.Errorf("close Pion UDP socket: %w", err))
		}
		api.closeErr = errors.Join(failures...)
	})
	return api.closeErr
}

func loopbackInterfaces() (map[string]struct{}, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("enumerate loopback interfaces: %w", err)
	}
	allowed := make(map[string]struct{})
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagUp == 0 || networkInterface.Flags&net.FlagLoopback == 0 {
			continue
		}
		addresses, addressErr := networkInterface.Addrs()
		if addressErr != nil {
			return nil, fmt.Errorf("enumerate addresses for loopback interface %q: %w", networkInterface.Name, addressErr)
		}
		for _, address := range addresses {
			if isExactLoopbackIPv4(networkAddressIP(address)) {
				allowed[networkInterface.Name] = struct{}{}
				break
			}
		}
	}
	if len(allowed) == 0 {
		return nil, errors.New("no active interface owns the IPv4 loopback address")
	}
	return allowed, nil
}

func networkAddressIP(address net.Addr) net.IP {
	switch value := address.(type) {
	case *net.IPNet:
		return value.IP
	case *net.IPAddr:
		return value.IP
	default:
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil {
			return ip
		}
		return net.ParseIP(address.String())
	}
}

func isExactLoopbackIPv4(address net.IP) bool {
	return address != nil && address.To4() != nil && address.To4().Equal(loopbackIPv4)
}
