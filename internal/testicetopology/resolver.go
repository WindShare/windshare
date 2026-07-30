package testicetopology

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
)

var (
	ErrResolveTopology     = errors.New("resolve test ICE topology")
	ErrProbeSource         = errors.New("probe topology source")
	ErrProbeConsensus      = errors.New("establish topology source consensus")
	ErrInventoryInterfaces = errors.New("inventory topology interfaces")
	ErrInterfaceOwnership  = errors.New("resolve topology interface ownership")
)

type SourceProber interface {
	ProbeSource(context.Context, ProbeDestination) (netip.Addr, error)
}

type InterfaceInventory interface {
	Interfaces() ([]InterfaceSnapshot, error)
}

type InterfaceSnapshot struct {
	Index                  uint32
	Name                   string
	Up                     bool
	Loopback               bool
	Addresses              []netip.Prefix
	AddressesWithoutPrefix []netip.Addr
}

type Resolver struct {
	prober    SourceProber
	inventory InterfaceInventory
}

func NewResolver(prober SourceProber, inventory InterfaceInventory) Resolver {
	return Resolver{prober: prober, inventory: inventory}
}

func NewStandardResolver() Resolver {
	network := NewStandardNetwork()
	return NewResolver(&network, &network)
}

func (resolver Resolver) Resolve(ctx context.Context, profile Profile) (Resolution, error) {
	if err := profile.Validate(); err != nil {
		return Resolution{}, errors.Join(ErrResolveTopology, err)
	}
	if resolver.prober == nil {
		return Resolution{}, errors.Join(
			ErrResolveTopology,
			ErrProbeSource,
			fmt.Errorf("source prober is required"),
		)
	}
	if resolver.inventory == nil {
		return Resolution{}, errors.Join(
			ErrResolveTopology,
			ErrInventoryInterfaces,
			fmt.Errorf("interface inventory is required"),
		)
	}
	profileSHA256, err := profile.SHA256()
	if err != nil {
		return Resolution{}, errors.Join(ErrResolveTopology, err)
	}

	probeResults := make([]ProbeResult, 0, len(profile.SourceSelector.ProbeDestinations))
	var selected netip.Addr
	for index, destination := range profile.SourceSelector.ProbeDestinations {
		if err := ctx.Err(); err != nil {
			return Resolution{}, errors.Join(ErrResolveTopology, ErrProbeSource, err)
		}
		source, probeErr := resolver.prober.ProbeSource(ctx, destination)
		if probeErr != nil {
			return Resolution{}, errors.Join(
				ErrResolveTopology,
				ErrProbeSource,
				fmt.Errorf("probe %d: %w", index, probeErr),
			)
		}
		if err := ctx.Err(); err != nil {
			return Resolution{}, errors.Join(ErrResolveTopology, ErrProbeSource, err)
		}
		source = source.Unmap()
		if !source.IsValid() || !source.Is4() || !IsOperationalIPv4Unicast(source.String()) {
			return Resolution{}, errors.Join(
				ErrResolveTopology,
				ErrProbeSource,
				fmt.Errorf("probe %d selected a non-operational IPv4 source", index),
			)
		}
		if index == 0 {
			selected = source
		} else if source != selected {
			return Resolution{}, errors.Join(
				ErrResolveTopology,
				ErrProbeConsensus,
				fmt.Errorf("route probes disagree between %s and %s", selected, source),
			)
		}
		probeResults = append(probeResults, ProbeResult{
			DestinationAddress: destination.Address,
			DestinationPort:    destination.Port,
			SourceAddress:      source.String(),
		})
	}

	interfaces, err := resolver.inventory.Interfaces()
	if err != nil {
		return Resolution{}, errors.Join(
			ErrResolveTopology,
			ErrInventoryInterfaces,
			fmt.Errorf("inventory interfaces: %w", err),
		)
	}
	owners := make([]InterfaceSnapshot, 0, 1)
	for _, candidate := range interfaces {
		if !candidate.Up || candidate.Loopback || !snapshotOwns(candidate, selected) {
			continue
		}
		owners = append(owners, candidate)
	}
	if len(owners) != 1 {
		return Resolution{}, errors.Join(
			ErrResolveTopology,
			ErrInterfaceOwnership,
			fmt.Errorf("selected source has %d operational interface owners", len(owners)),
		)
	}
	owner := owners[0]
	eligible, err := eligibleAddresses(owner)
	if err != nil {
		return Resolution{}, errors.Join(ErrResolveTopology, ErrInventoryInterfaces, err)
	}
	resolution := Resolution{
		TopologyResolutionSchemaVersion: ResolutionSchemaVersion,
		TopologyID:                      profile.TopologyID,
		TopologyProfileSHA256:           profileSHA256,
		SelectorAlgorithm:               profile.SourceSelector.Algorithm,
		AddressFamily:                   profile.AddressFamily,
		ProbeResults:                    probeResults,
		Interface: ResolvedInterface{
			Index:             owner.Index,
			Name:              owner.Name,
			SelectedAddress:   selected.String(),
			EligibleAddresses: eligible,
		},
	}
	if err := resolution.Validate(profile, profileSHA256); err != nil {
		return Resolution{}, errors.Join(ErrResolveTopology, ErrInterfaceOwnership, err)
	}
	return resolution, nil
}

func snapshotOwns(snapshot InterfaceSnapshot, selected netip.Addr) bool {
	for _, prefix := range snapshot.Addresses {
		if prefix.IsValid() && prefix.Addr().Unmap() == selected {
			return true
		}
	}
	for _, address := range snapshot.AddressesWithoutPrefix {
		if address.IsValid() && address.Unmap() == selected {
			return true
		}
	}
	return false
}

func eligibleAddresses(snapshot InterfaceSnapshot) ([]EligibleAddress, error) {
	if snapshot.Index == 0 || !validNFCText(snapshot.Name, maximumInterfaceNameBytes) {
		return nil, fmt.Errorf("resolved interface identity is invalid")
	}
	addresses := make([]EligibleAddress, 0, len(snapshot.Addresses))
	seen := make(map[string]uint8, len(snapshot.Addresses))
	for _, prefix := range snapshot.Addresses {
		if !prefix.IsValid() {
			return nil, fmt.Errorf("resolved interface contains an invalid address prefix")
		}
		address := prefix.Addr().Unmap()
		if !address.Is4() {
			continue
		}
		encoded := address.String()
		if !IsOperationalIPv4Unicast(encoded) {
			continue
		}
		bits := prefix.Bits()
		if bits < 1 || bits > 32 {
			return nil, fmt.Errorf("resolved interface address %s has invalid prefix length %d", encoded, bits)
		}
		prefixLength := uint8(bits)
		if previousPrefixLength, duplicate := seen[encoded]; duplicate {
			if previousPrefixLength == prefixLength {
				continue
			}
			return nil, fmt.Errorf(
				"resolved interface owns address %s with conflicting prefix lengths %d and %d",
				encoded,
				previousPrefixLength,
				prefixLength,
			)
		}
		seen[encoded] = prefixLength
		addresses = append(addresses, EligibleAddress{Address: encoded, PrefixLength: prefixLength})
	}
	sort.Slice(addresses, func(left, right int) bool {
		leftAddress, _ := ipv4Number(addresses[left].Address)
		rightAddress, _ := ipv4Number(addresses[right].Address)
		if leftAddress != rightAddress {
			return leftAddress < rightAddress
		}
		return addresses[left].PrefixLength < addresses[right].PrefixLength
	})
	return addresses, nil
}

type localUDPSocket interface {
	LocalAddr() net.Addr
	Close() error
}

type udpConnector interface {
	Connect(context.Context, netip.AddrPort) (localUDPSocket, error)
}

type standardUDPConnector struct {
	dialer *net.Dialer
}

func (connector standardUDPConnector) Connect(
	ctx context.Context,
	destination netip.AddrPort,
) (localUDPSocket, error) {
	if connector.dialer == nil {
		return nil, fmt.Errorf("standard UDP connector has no dialer")
	}
	connection, err := connector.dialer.DialUDP(ctx, "udp4", netip.AddrPort{}, destination)
	if err != nil {
		return nil, err
	}
	return connection, nil
}

type StandardNetwork struct {
	connector          udpConnector
	listInterfaces     func() ([]net.Interface, error)
	interfaceAddresses func(net.Interface) ([]net.Addr, error)
}

func NewStandardNetwork() StandardNetwork {
	return StandardNetwork{
		connector:      standardUDPConnector{dialer: &net.Dialer{}},
		listInterfaces: net.Interfaces,
		interfaceAddresses: func(networkInterface net.Interface) ([]net.Addr, error) {
			return networkInterface.Addrs()
		},
	}
}

// ProbeSource asks the kernel to connect a UDP socket and reads the selected
// local endpoint. It deliberately never writes: route selection is the evidence
// boundary, and sending probe traffic would add an external dependency.
func (network StandardNetwork) ProbeSource(
	ctx context.Context,
	destination ProbeDestination,
) (address netip.Addr, err error) {
	if network.connector == nil {
		return netip.Addr{}, fmt.Errorf("standard UDP source prober has no connector")
	}
	if !IsOperationalIPv4Unicast(destination.Address) || destination.Port == 0 {
		return netip.Addr{}, fmt.Errorf("invalid UDP route-probe destination")
	}
	remoteAddress, parseErr := netip.ParseAddr(destination.Address)
	if parseErr != nil {
		return netip.Addr{}, fmt.Errorf("parse UDP route-probe destination: %w", parseErr)
	}
	connection, err := network.connector.Connect(
		ctx,
		netip.AddrPortFrom(remoteAddress, destination.Port),
	)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("connect UDP route probe: %w", err)
	}
	if connection == nil {
		return netip.Addr{}, fmt.Errorf("connect UDP route probe: connector returned no socket")
	}
	defer func() {
		if closeErr := connection.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close UDP route probe: %w", closeErr))
		}
	}()
	local, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok || local == nil {
		return netip.Addr{}, fmt.Errorf("UDP route probe returned a non-UDP local endpoint")
	}
	selected, ok := netip.AddrFromSlice(local.IP)
	if !ok {
		return netip.Addr{}, fmt.Errorf("UDP route probe returned an invalid local address")
	}
	selected = selected.Unmap()
	if !selected.Is4() || !IsOperationalIPv4Unicast(selected.String()) {
		return netip.Addr{}, fmt.Errorf("UDP route probe returned a non-operational IPv4 source")
	}
	return selected, nil
}

func (network StandardNetwork) Interfaces() ([]InterfaceSnapshot, error) {
	if network.listInterfaces == nil || network.interfaceAddresses == nil {
		return nil, fmt.Errorf("standard interface inventory is not configured")
	}
	interfaces, err := network.listInterfaces()
	if err != nil {
		return nil, fmt.Errorf("list network interfaces: %w", err)
	}
	snapshots := make([]InterfaceSnapshot, 0, len(interfaces))
	for _, networkInterface := range interfaces {
		if networkInterface.Index < 1 || uint64(networkInterface.Index) > uint64(^uint32(0)) {
			return nil, fmt.Errorf("network interface %q has an invalid index", networkInterface.Name)
		}
		addresses, addressErr := network.interfaceAddresses(networkInterface)
		if addressErr != nil {
			return nil, fmt.Errorf("list addresses for interface %q: %w", networkInterface.Name, addressErr)
		}
		prefixes := make([]netip.Prefix, 0, len(addresses))
		addressesWithoutPrefix := make([]netip.Addr, 0, len(addresses))
		for _, address := range addresses {
			parsed, parseErr := parseInterfaceAddress(address)
			if parseErr != nil {
				return nil, fmt.Errorf("parse address for interface %q: %w", networkInterface.Name, parseErr)
			}
			if parsed.Prefix.IsValid() {
				prefixes = append(prefixes, parsed.Prefix)
			}
			if parsed.AddressWithoutPrefix.IsValid() {
				addressesWithoutPrefix = append(addressesWithoutPrefix, parsed.AddressWithoutPrefix)
			}
		}
		snapshots = append(snapshots, InterfaceSnapshot{
			Index:                  uint32(networkInterface.Index),
			Name:                   networkInterface.Name,
			Up:                     networkInterface.Flags&net.FlagUp != 0,
			Loopback:               networkInterface.Flags&net.FlagLoopback != 0,
			Addresses:              prefixes,
			AddressesWithoutPrefix: addressesWithoutPrefix,
		})
	}
	return snapshots, nil
}

type parsedInterfaceAddress struct {
	Prefix               netip.Prefix
	AddressWithoutPrefix netip.Addr
}

func parseInterfaceAddress(address net.Addr) (parsedInterfaceAddress, error) {
	switch value := address.(type) {
	case *net.IPNet:
		if value == nil {
			return parsedInterfaceAddress{}, fmt.Errorf("invalid nil IP network")
		}
		parsed, ok := netip.AddrFromSlice(value.IP)
		if !ok {
			return parsedInterfaceAddress{}, fmt.Errorf("invalid IP address")
		}
		parsed = parsed.Unmap()
		if !parsed.Is4() {
			return parsedInterfaceAddress{}, nil
		}
		ones, bits := value.Mask.Size()
		if bits != 32 || ones < 0 {
			return parsedInterfaceAddress{}, fmt.Errorf("invalid IPv4 network mask")
		}
		return parsedInterfaceAddress{Prefix: netip.PrefixFrom(parsed, ones)}, nil
	case *net.IPAddr:
		if value == nil {
			return parsedInterfaceAddress{}, fmt.Errorf("invalid nil IP address")
		}
		parsed, ok := netip.AddrFromSlice(value.IP)
		if !ok {
			return parsedInterfaceAddress{}, fmt.Errorf("invalid IP address")
		}
		parsed = parsed.Unmap()
		if !parsed.Is4() {
			return parsedInterfaceAddress{}, nil
		}
		// Some Windows adapters report ownership without a mask. Keeping that
		// evidence avoids a false "no owner" result, while omission from the
		// eligible set prevents us from inventing a /32 assignment.
		return parsedInterfaceAddress{AddressWithoutPrefix: parsed}, nil
	default:
		return parsedInterfaceAddress{}, fmt.Errorf("unsupported network address type %T", address)
	}
}
