package browsermatrixpion

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/pion/stun/v3"
)

const defaultSTUNProbeTimeout = 10 * time.Second

var nonPublicSTUNPrefixes = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

type STUNAddressPolicy func(netip.Addr) bool

type RealSTUNProber struct {
	Dialer        *net.Dialer
	AddressPolicy STUNAddressPolicy
}

func (prober RealSTUNProber) Probe(ctx context.Context, rawURI string) error {
	uri, err := stun.ParseURI(rawURI)
	if err != nil || uri.Scheme != stun.SchemeTypeSTUN || uri.Proto != stun.ProtoTypeUDP {
		return errors.New("STUN probe endpoint is invalid")
	}
	dialer := prober.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	conn, err := dialer.DialContext(ctx, "udp", net.JoinHostPort(uri.Host, strconv.Itoa(uri.Port)))
	if err != nil {
		return errors.New("STUN probe dial failed")
	}
	policy := prober.AddressPolicy
	if policy == nil {
		policy = publicSTUNAddress
	}
	remote, ok := conn.RemoteAddr().(*net.UDPAddr)
	if !ok {
		_ = conn.Close()
		return errors.New("STUN probe endpoint is not publicly routed")
	}
	remoteAddress, addressOK := netip.AddrFromSlice(remote.IP)
	if !addressOK || !policy(remoteAddress.Unmap()) {
		_ = conn.Close()
		return errors.New("STUN probe endpoint is not publicly routed")
	}
	deadline := time.Now().Add(defaultSTUNProbeTimeout)
	if contextualDeadline, ok := ctx.Deadline(); ok && contextualDeadline.Before(deadline) {
		deadline = contextualDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return errors.New("STUN probe deadline binding failed")
	}
	client, err := stun.NewClient(conn)
	if err != nil {
		_ = conn.Close()
		return errors.New("STUN probe client failed")
	}
	defer func() { _ = client.Close() }()

	var observed atomic.Bool
	err = client.Do(stun.MustBuild(stun.TransactionID, stun.BindingRequest), func(event stun.Event) {
		if event.Error != nil || event.Message == nil {
			return
		}
		var mapped stun.XORMappedAddress
		if getErr := mapped.GetFrom(event.Message); getErr != nil || mapped.Port < 1 {
			return
		}
		address, parseErr := netip.ParseAddr(mapped.IP.String())
		observed.Store(parseErr == nil && policy(address.Unmap()))
	})
	if err != nil || !observed.Load() {
		return errors.New("STUN probe did not observe a server-reflexive address")
	}
	return nil
}

func publicSTUNAddress(address netip.Addr) bool {
	if !address.IsValid() || !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicSTUNPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
