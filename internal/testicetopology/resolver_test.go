package testicetopology

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"testing"
)

type sourceProberFunc func(context.Context, ProbeDestination) (netip.Addr, error)

func (function sourceProberFunc) ProbeSource(
	ctx context.Context,
	destination ProbeDestination,
) (netip.Addr, error) {
	return function(ctx, destination)
}

type interfaceInventoryFunc func() ([]InterfaceSnapshot, error)

func (function interfaceInventoryFunc) Interfaces() ([]InterfaceSnapshot, error) {
	return function()
}

type udpConnectorFunc func(context.Context, netip.AddrPort) (localUDPSocket, error)

func (function udpConnectorFunc) Connect(
	ctx context.Context,
	destination netip.AddrPort,
) (localUDPSocket, error) {
	return function(ctx, destination)
}

type recordingUDPSocket struct {
	local    net.Addr
	closeErr error
	closed   bool
}

func (socket *recordingUDPSocket) LocalAddr() net.Addr {
	return socket.local
}

func (socket *recordingUDPSocket) Close() error {
	socket.closed = true
	return socket.closeErr
}

type unsupportedNetworkAddress struct{}

func (unsupportedNetworkAddress) Network() string { return "unsupported" }
func (unsupportedNetworkAddress) String() string  { return "unsupported" }

func TestResolverFreezesUnanimousKernelSelection(t *testing.T) {
	t.Parallel()
	profile := loadSharedProfile(t)
	selected := netip.MustParseAddr("192.0.2.10")
	returnedSources := []netip.Addr{
		netip.MustParseAddr("::ffff:192.0.2.10"),
		selected,
		selected,
	}
	var observedDestinations []ProbeDestination
	prober := sourceProberFunc(func(_ context.Context, destination ProbeDestination) (netip.Addr, error) {
		observedDestinations = append(observedDestinations, destination)
		return returnedSources[len(observedDestinations)-1], nil
	})
	inventoryCalls := 0
	inventory := interfaceInventoryFunc(func() ([]InterfaceSnapshot, error) {
		inventoryCalls++
		return []InterfaceSnapshot{
			{
				Index:     1,
				Name:      "down-owner",
				Addresses: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/24")},
			},
			{
				Index:     2,
				Name:      "loopback-owner",
				Up:        true,
				Loopback:  true,
				Addresses: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/24")},
			},
			{
				Index:    7,
				Name:     "test-uplink0",
				Up:       true,
				Loopback: false,
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("192.0.2.11/24"),
					netip.MustParsePrefix("192.0.2.10/24"),
					netip.MustParsePrefix("192.0.2.10/24"),
					netip.MustParsePrefix("169.254.1.1/16"),
					netip.MustParsePrefix("2001:db8::1/64"),
				},
			},
		}, nil
	})

	resolution, err := NewResolver(prober, inventory).Resolve(context.Background(), profile)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !reflect.DeepEqual(observedDestinations, profile.SourceSelector.ProbeDestinations) {
		t.Fatalf("probe order = %+v, want %+v", observedDestinations, profile.SourceSelector.ProbeDestinations)
	}
	if inventoryCalls != 1 {
		t.Fatalf("inventory calls = %d, want 1", inventoryCalls)
	}
	wantEligible := []EligibleAddress{
		{Address: "192.0.2.10", PrefixLength: 24},
		{Address: "192.0.2.11", PrefixLength: 24},
	}
	if resolution.TopologyProfileSHA256 != sharedProfileSHA256 ||
		resolution.Interface.Index != 7 ||
		resolution.Interface.Name != "test-uplink0" ||
		resolution.Interface.SelectedAddress != selected.String() ||
		!reflect.DeepEqual(resolution.Interface.EligibleAddresses, wantEligible) {
		t.Fatalf("resolution = %+v", resolution)
	}
	if len(resolution.ProbeResults) != len(profile.SourceSelector.ProbeDestinations) {
		t.Fatalf("probe results = %+v", resolution.ProbeResults)
	}
	for index, result := range resolution.ProbeResults {
		if result.DestinationAddress != profile.SourceSelector.ProbeDestinations[index].Address ||
			result.DestinationPort != profile.SourceSelector.ProbeDestinations[index].Port ||
			result.SourceAddress != selected.String() {
			t.Fatalf("probe result %d = %+v", index, result)
		}
	}
	if err := resolution.Validate(profile, sharedProfileSHA256); err != nil {
		t.Fatalf("Validate resolution: %v", err)
	}
}

func TestResolverFailureCategories(t *testing.T) {
	t.Parallel()
	profile := loadSharedProfile(t)
	selected := netip.MustParseAddr("192.0.2.10")
	validProber := sourceProberFunc(func(context.Context, ProbeDestination) (netip.Addr, error) {
		return selected, nil
	})
	validInventory := interfaceInventoryFunc(func() ([]InterfaceSnapshot, error) {
		return []InterfaceSnapshot{{
			Index:     7,
			Name:      "test-uplink0",
			Up:        true,
			Addresses: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/24")},
		}}, nil
	})
	probeFailure := errors.New("probe failed")
	inventoryFailure := errors.New("inventory failed")
	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		run  func() error
		want []error
	}{
		{
			name: "invalid profile",
			run: func() error {
				_, err := NewResolver(validProber, validInventory).Resolve(context.Background(), Profile{})
				return err
			},
			want: []error{ErrInvalidProfile},
		},
		{
			name: "missing prober",
			run: func() error {
				_, err := NewResolver(nil, validInventory).Resolve(context.Background(), profile)
				return err
			},
			want: []error{ErrProbeSource},
		},
		{
			name: "missing inventory",
			run: func() error {
				_, err := NewResolver(validProber, nil).Resolve(context.Background(), profile)
				return err
			},
			want: []error{ErrInventoryInterfaces},
		},
		{
			name: "cancelled before probing",
			run: func() error {
				_, err := NewResolver(validProber, validInventory).Resolve(cancelledContext, profile)
				return err
			},
			want: []error{ErrProbeSource, context.Canceled},
		},
		{
			name: "cancelled while probing",
			run: func() error {
				ctx, cancelDuringProbe := context.WithCancel(context.Background())
				prober := sourceProberFunc(func(context.Context, ProbeDestination) (netip.Addr, error) {
					cancelDuringProbe()
					return selected, nil
				})
				_, err := NewResolver(prober, validInventory).Resolve(ctx, profile)
				return err
			},
			want: []error{ErrProbeSource, context.Canceled},
		},
		{
			name: "probe failure",
			run: func() error {
				prober := sourceProberFunc(func(context.Context, ProbeDestination) (netip.Addr, error) {
					return netip.Addr{}, probeFailure
				})
				_, err := NewResolver(prober, validInventory).Resolve(context.Background(), profile)
				return err
			},
			want: []error{ErrProbeSource, probeFailure},
		},
		{
			name: "invalid probe source",
			run: func() error {
				prober := sourceProberFunc(func(context.Context, ProbeDestination) (netip.Addr, error) {
					return netip.MustParseAddr("127.0.0.1"), nil
				})
				_, err := NewResolver(prober, validInventory).Resolve(context.Background(), profile)
				return err
			},
			want: []error{ErrProbeSource},
		},
		{
			name: "probe disagreement",
			run: func() error {
				calls := 0
				prober := sourceProberFunc(func(context.Context, ProbeDestination) (netip.Addr, error) {
					calls++
					if calls == 2 {
						return netip.MustParseAddr("192.0.2.11"), nil
					}
					return selected, nil
				})
				_, err := NewResolver(prober, validInventory).Resolve(context.Background(), profile)
				return err
			},
			want: []error{ErrProbeConsensus},
		},
		{
			name: "inventory failure",
			run: func() error {
				inventory := interfaceInventoryFunc(func() ([]InterfaceSnapshot, error) {
					return nil, inventoryFailure
				})
				_, err := NewResolver(validProber, inventory).Resolve(context.Background(), profile)
				return err
			},
			want: []error{ErrInventoryInterfaces, inventoryFailure},
		},
		{
			name: "no owner",
			run: func() error {
				inventory := interfaceInventoryFunc(func() ([]InterfaceSnapshot, error) {
					return []InterfaceSnapshot{{
						Index: 1, Name: "other", Up: true,
						Addresses: []netip.Prefix{netip.MustParsePrefix("192.0.2.11/24")},
					}}, nil
				})
				_, err := NewResolver(validProber, inventory).Resolve(context.Background(), profile)
				return err
			},
			want: []error{ErrInterfaceOwnership},
		},
		{
			name: "ambiguous owner",
			run: func() error {
				owner := InterfaceSnapshot{
					Index: 1, Name: "owner", Up: true,
					Addresses: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/24")},
				}
				otherOwner := owner
				otherOwner.Index = 2
				otherOwner.Name = "other-owner"
				inventory := interfaceInventoryFunc(func() ([]InterfaceSnapshot, error) {
					return []InterfaceSnapshot{owner, otherOwner}, nil
				})
				_, err := NewResolver(validProber, inventory).Resolve(context.Background(), profile)
				return err
			},
			want: []error{ErrInterfaceOwnership},
		},
		{
			name: "prefixless ownership cannot invent assignment",
			run: func() error {
				inventory := interfaceInventoryFunc(func() ([]InterfaceSnapshot, error) {
					return []InterfaceSnapshot{{
						Index: 7, Name: "windows-uplink", Up: true,
						AddressesWithoutPrefix: []netip.Addr{selected},
					}}, nil
				})
				_, err := NewResolver(validProber, inventory).Resolve(context.Background(), profile)
				return err
			},
			want: []error{ErrInterfaceOwnership, ErrInvalidResolution},
		},
		{
			name: "conflicting prefixes",
			run: func() error {
				inventory := interfaceInventoryFunc(func() ([]InterfaceSnapshot, error) {
					return []InterfaceSnapshot{{
						Index: 7, Name: "test-uplink0", Up: true,
						Addresses: []netip.Prefix{
							netip.MustParsePrefix("192.0.2.10/24"),
							netip.MustParsePrefix("192.0.2.10/25"),
						},
					}}, nil
				})
				_, err := NewResolver(validProber, inventory).Resolve(context.Background(), profile)
				return err
			},
			want: []error{ErrInventoryInterfaces},
		},
		{
			name: "invalid owner identity",
			run: func() error {
				inventory := interfaceInventoryFunc(func() ([]InterfaceSnapshot, error) {
					return []InterfaceSnapshot{{
						Index: 7, Name: "", Up: true,
						Addresses: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/24")},
					}}, nil
				})
				_, err := NewResolver(validProber, inventory).Resolve(context.Background(), profile)
				return err
			},
			want: []error{ErrInventoryInterfaces},
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := testCase.run()
			if !errors.Is(err, ErrResolveTopology) {
				t.Fatalf("Resolve error = %v, want ErrResolveTopology", err)
			}
			for _, expected := range testCase.want {
				if !errors.Is(err, expected) {
					t.Fatalf("Resolve error = %v, want %v", err, expected)
				}
			}
		})
	}
}

func TestStandardNetworkProbeSourceUsesNoWriteSocketBoundary(t *testing.T) {
	t.Parallel()
	wantDestination := netip.MustParseAddrPort("192.0.2.1:9")
	socket := &recordingUDPSocket{local: &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 49152}}
	connectCalls := 0
	network := StandardNetwork{
		connector: udpConnectorFunc(func(_ context.Context, destination netip.AddrPort) (localUDPSocket, error) {
			connectCalls++
			if destination != wantDestination {
				t.Fatalf("destination = %s, want %s", destination, wantDestination)
			}
			return socket, nil
		}),
	}
	selected, err := network.ProbeSource(context.Background(), ProbeDestination{Address: "192.0.2.1", Port: 9})
	if err != nil {
		t.Fatalf("ProbeSource: %v", err)
	}
	if selected != netip.MustParseAddr("192.0.2.10") || connectCalls != 1 || !socket.closed {
		t.Fatalf("selected = %s, calls = %d, closed = %v", selected, connectCalls, socket.closed)
	}
}

func TestStandardNetworkProbeSourceRejectsInvalidEndpoints(t *testing.T) {
	t.Parallel()
	connectFailure := errors.New("connect failed")
	closeFailure := errors.New("close failed")
	tests := []struct {
		name        string
		network     StandardNetwork
		destination ProbeDestination
		want        error
	}{
		{
			name:        "missing connector",
			destination: ProbeDestination{Address: "192.0.2.1", Port: 9},
		},
		{
			name: "zero port",
			network: StandardNetwork{connector: udpConnectorFunc(func(context.Context, netip.AddrPort) (localUDPSocket, error) {
				t.Fatal("connector called for invalid destination")
				return nil, nil
			})},
			destination: ProbeDestination{Address: "192.0.2.1"},
		},
		{
			name: "non-operational destination",
			network: StandardNetwork{connector: udpConnectorFunc(func(context.Context, netip.AddrPort) (localUDPSocket, error) {
				t.Fatal("connector called for invalid destination")
				return nil, nil
			})},
			destination: ProbeDestination{Address: "127.0.0.1", Port: 9},
		},
		{
			name: "connect failure",
			network: StandardNetwork{connector: udpConnectorFunc(func(context.Context, netip.AddrPort) (localUDPSocket, error) {
				return nil, connectFailure
			})},
			destination: ProbeDestination{Address: "192.0.2.1", Port: 9},
			want:        connectFailure,
		},
		{
			name: "missing socket",
			network: StandardNetwork{connector: udpConnectorFunc(func(context.Context, netip.AddrPort) (localUDPSocket, error) {
				return nil, nil
			})},
			destination: ProbeDestination{Address: "192.0.2.1", Port: 9},
		},
		{
			name: "non-UDP local endpoint",
			network: StandardNetwork{connector: udpConnectorFunc(func(context.Context, netip.AddrPort) (localUDPSocket, error) {
				return &recordingUDPSocket{local: unsupportedNetworkAddress{}}, nil
			})},
			destination: ProbeDestination{Address: "192.0.2.1", Port: 9},
		},
		{
			name: "invalid local IP",
			network: StandardNetwork{connector: udpConnectorFunc(func(context.Context, netip.AddrPort) (localUDPSocket, error) {
				return &recordingUDPSocket{local: &net.UDPAddr{IP: net.IP{1, 2, 3}}}, nil
			})},
			destination: ProbeDestination{Address: "192.0.2.1", Port: 9},
		},
		{
			name: "IPv6 local IP",
			network: StandardNetwork{connector: udpConnectorFunc(func(context.Context, netip.AddrPort) (localUDPSocket, error) {
				return &recordingUDPSocket{local: &net.UDPAddr{IP: net.ParseIP("2001:db8::1")}}, nil
			})},
			destination: ProbeDestination{Address: "192.0.2.1", Port: 9},
		},
		{
			name: "non-operational local IP",
			network: StandardNetwork{connector: udpConnectorFunc(func(context.Context, netip.AddrPort) (localUDPSocket, error) {
				return &recordingUDPSocket{local: &net.UDPAddr{IP: net.ParseIP("127.0.0.1")}}, nil
			})},
			destination: ProbeDestination{Address: "192.0.2.1", Port: 9},
		},
		{
			name: "close failure",
			network: StandardNetwork{connector: udpConnectorFunc(func(context.Context, netip.AddrPort) (localUDPSocket, error) {
				return &recordingUDPSocket{
					local:    &net.UDPAddr{IP: net.ParseIP("192.0.2.10")},
					closeErr: closeFailure,
				}, nil
			})},
			destination: ProbeDestination{Address: "192.0.2.1", Port: 9},
			want:        closeFailure,
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := testCase.network.ProbeSource(context.Background(), testCase.destination)
			if err == nil {
				t.Fatal("ProbeSource succeeded")
			}
			if testCase.want != nil && !errors.Is(err, testCase.want) {
				t.Fatalf("ProbeSource error = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestStandardNetworkInterfacesPreservePrefixlessOwnership(t *testing.T) {
	t.Parallel()
	network := StandardNetwork{
		listInterfaces: func() ([]net.Interface, error) {
			return []net.Interface{
				{Index: 7, Name: "uplink", Flags: net.FlagUp},
				{Index: 8, Name: "loopback", Flags: net.FlagUp | net.FlagLoopback},
			}, nil
		},
		interfaceAddresses: func(networkInterface net.Interface) ([]net.Addr, error) {
			if networkInterface.Index == 8 {
				return nil, nil
			}
			return []net.Addr{
				&net.IPNet{IP: net.ParseIP("192.0.2.10").To4(), Mask: net.CIDRMask(24, 32)},
				&net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)},
				&net.IPAddr{IP: net.ParseIP("198.51.100.10")},
				&net.IPAddr{IP: net.ParseIP("2001:db8::2")},
			}, nil
		},
	}
	snapshots, err := network.Interfaces()
	if err != nil {
		t.Fatalf("Interfaces: %v", err)
	}
	want := []InterfaceSnapshot{
		{
			Index:                  7,
			Name:                   "uplink",
			Up:                     true,
			Addresses:              []netip.Prefix{netip.MustParsePrefix("192.0.2.10/24")},
			AddressesWithoutPrefix: []netip.Addr{netip.MustParseAddr("198.51.100.10")},
		},
		{
			Index:                  8,
			Name:                   "loopback",
			Up:                     true,
			Loopback:               true,
			Addresses:              []netip.Prefix{},
			AddressesWithoutPrefix: []netip.Addr{},
		},
	}
	if !reflect.DeepEqual(snapshots, want) {
		t.Fatalf("snapshots = %+v, want %+v", snapshots, want)
	}
}

func TestStandardNetworkInterfacesFailClosed(t *testing.T) {
	t.Parallel()
	listFailure := errors.New("list failed")
	addressFailure := errors.New("addresses failed")
	tests := []struct {
		name    string
		network StandardNetwork
		want    error
	}{
		{name: "missing functions", network: StandardNetwork{}},
		{
			name: "list failure",
			network: StandardNetwork{
				listInterfaces:     func() ([]net.Interface, error) { return nil, listFailure },
				interfaceAddresses: func(net.Interface) ([]net.Addr, error) { return nil, nil },
			},
			want: listFailure,
		},
		{
			name: "invalid index",
			network: StandardNetwork{
				listInterfaces: func() ([]net.Interface, error) {
					return []net.Interface{{Index: 0, Name: "invalid"}}, nil
				},
				interfaceAddresses: func(net.Interface) ([]net.Addr, error) { return nil, nil },
			},
		},
		{
			name: "address failure",
			network: StandardNetwork{
				listInterfaces: func() ([]net.Interface, error) {
					return []net.Interface{{Index: 1, Name: "uplink"}}, nil
				},
				interfaceAddresses: func(net.Interface) ([]net.Addr, error) { return nil, addressFailure },
			},
			want: addressFailure,
		},
		{
			name: "unsupported address",
			network: StandardNetwork{
				listInterfaces: func() ([]net.Interface, error) {
					return []net.Interface{{Index: 1, Name: "uplink"}}, nil
				},
				interfaceAddresses: func(net.Interface) ([]net.Addr, error) {
					return []net.Addr{unsupportedNetworkAddress{}}, nil
				},
			},
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := testCase.network.Interfaces()
			if err == nil {
				t.Fatal("Interfaces succeeded")
			}
			if testCase.want != nil && !errors.Is(err, testCase.want) {
				t.Fatalf("Interfaces error = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestParseInterfaceAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		address           net.Addr
		wantPrefix        netip.Prefix
		wantWithoutPrefix netip.Addr
		wantError         bool
	}{
		{
			name:       "IPv4 network",
			address:    &net.IPNet{IP: net.ParseIP("192.0.2.10").To4(), Mask: net.CIDRMask(24, 32)},
			wantPrefix: netip.MustParsePrefix("192.0.2.10/24"),
		},
		{
			name:    "IPv6 network ignored",
			address: &net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)},
		},
		{
			name:              "IPv4 address without prefix",
			address:           &net.IPAddr{IP: net.ParseIP("192.0.2.10")},
			wantWithoutPrefix: netip.MustParseAddr("192.0.2.10"),
		},
		{name: "IPv6 address ignored", address: &net.IPAddr{IP: net.ParseIP("2001:db8::1")}},
		{name: "nil IP network", address: (*net.IPNet)(nil), wantError: true},
		{name: "nil IP address", address: (*net.IPAddr)(nil), wantError: true},
		{name: "invalid network IP", address: &net.IPNet{}, wantError: true},
		{name: "invalid address IP", address: &net.IPAddr{}, wantError: true},
		{
			name:      "invalid IPv4 mask",
			address:   &net.IPNet{IP: net.ParseIP("192.0.2.10").To4(), Mask: net.IPMask{255, 0, 255, 0}},
			wantError: true,
		},
		{name: "unsupported", address: unsupportedNetworkAddress{}, wantError: true},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := parseInterfaceAddress(testCase.address)
			if (err != nil) != testCase.wantError {
				t.Fatalf("parseInterfaceAddress error = %v, wantError %v", err, testCase.wantError)
			}
			if err == nil && (parsed.Prefix != testCase.wantPrefix ||
				parsed.AddressWithoutPrefix != testCase.wantWithoutPrefix) {
				t.Fatalf("parsed = %+v, want prefix %s without-prefix %s", parsed, testCase.wantPrefix, testCase.wantWithoutPrefix)
			}
		})
	}
}

func TestStandardConstructorsWireConcreteAdapters(t *testing.T) {
	t.Parallel()
	network := NewStandardNetwork()
	if network.connector == nil || network.listInterfaces == nil || network.interfaceAddresses == nil {
		t.Fatalf("NewStandardNetwork = %+v", network)
	}
	resolver := NewStandardResolver()
	if resolver.prober == nil || resolver.inventory == nil {
		t.Fatalf("NewStandardResolver = %+v", resolver)
	}
	connector := standardUDPConnector{}
	if _, err := connector.Connect(context.Background(), netip.MustParseAddrPort("192.0.2.1:9")); err == nil {
		t.Fatal("nil standard UDP dialer connected")
	}
}

func TestEligibleAddressesRejectInvalidPrefix(t *testing.T) {
	t.Parallel()
	tests := []netip.Prefix{
		{},
		netip.MustParsePrefix("192.0.2.10/0"),
	}
	for _, prefix := range tests {
		_, err := eligibleAddresses(InterfaceSnapshot{
			Index: 7, Name: "uplink", Addresses: []netip.Prefix{prefix},
		})
		if err == nil {
			t.Fatalf("eligibleAddresses(%s) succeeded", prefix)
		}
	}
	if _, err := eligibleAddresses(InterfaceSnapshot{Index: 0, Name: "uplink"}); err == nil {
		t.Fatal("eligibleAddresses accepted zero interface index")
	}
}

func ExampleResolver_Resolve() {
	fmt.Println(ErrResolveTopology)
	// Output: resolve test ICE topology
}
