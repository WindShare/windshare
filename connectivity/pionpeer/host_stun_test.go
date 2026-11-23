package pionpeer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

type stubHostBinding func(context.Context, netip.AddrPort, netip.AddrPort, time.Duration) (netip.AddrPort, error)

func (probe stubHostBinding) DiscoverXORMappedAddr(ctx context.Context, local netip.AddrPort, server *net.UDPAddr, timeout time.Duration) (netip.AddrPort, error) {
	return probe(ctx, local, server.AddrPort(), timeout)
}

func TestHostSTUNFallsBackAfterBlackholedDNSAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		server := iceconfig.STUNServer{Host: "stun.example", Port: 3478}
		local := netip.MustParseAddrPort("192.0.2.10:23500")
		first := netip.MustParseAddrPort("192.0.2.1:3478")
		second := netip.MustParseAddrPort("192.0.2.2:3478")
		mapped := netip.MustParseAddrPort("198.51.100.9:42000")
		lookup := func(ctx context.Context, network, host string) ([]netip.Addr, error) {
			if network != "ip4" || host != server.Host {
				t.Fatalf("unexpected lookup %s %s", network, host)
			}
			if deadline, ok := ctx.Deadline(); !ok || deadline.Sub(start) != hostSTUNTimeout {
				t.Fatalf("DNS deadline = %v, want %v", deadline, start.Add(hostSTUNTimeout))
			}
			time.Sleep(time.Second)
			return []netip.Addr{
				netip.MustParseAddr("2001:db8::1"), netip.IPv4Unspecified(), first.Addr(),
				netip.MustParseAddr("::ffff:192.0.2.1"), netip.MustParseAddr("224.0.0.1"), second.Addr(),
			}, nil
		}

		var probed []netip.AddrPort
		probe := stubHostBinding(func(_ context.Context, socket, target netip.AddrPort, timeout time.Duration) (netip.AddrPort, error) {
			if socket != local {
				t.Fatalf("probe used %v, want canonical host socket %v", socket, local)
			}
			probed = append(probed, target)
			remaining := time.Until(start.Add(hostSTUNTimeout))
			switch target {
			case first:
				if timeout <= 0 || timeout >= remaining {
					t.Fatalf("first address got %v of %v left after DNS", timeout, remaining)
				}
				time.Sleep(timeout)
				return netip.AddrPort{}, context.DeadlineExceeded
			case second:
				if timeout <= 0 || timeout != remaining {
					t.Fatalf("fallback address got %v, want remaining %v", timeout, remaining)
				}
				return mapped, nil
			default:
				t.Fatalf("unexpected probe target %v", target)
				return netip.AddrPort{}, nil
			}
		})
		input := netip.MustParseAddrPort("[::ffff:192.0.2.10]:23500")
		got, err := discoverHostSTUNWithLookup(t.Context(), probe, input, server, false, lookup)
		if err != nil || got != mapped || !slices.Equal(probed, []netip.AddrPort{first, second}) {
			t.Fatalf("mapped = %v, error = %v, probe order = %v", got, err, probed)
		}
	})
}

func TestHostSTUNDNSHonorsEarlierCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = time.Second
		start := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), budget)
		defer cancel()
		lookup := func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		got, err := discoverHostSTUNWithLookup(ctx, nil, netip.MustParseAddrPort("[2001:db8::1]:1234"),
			iceconfig.STUNServer{Host: "stun.example", Port: 3478}, false, lookup)
		if !errors.Is(err, context.DeadlineExceeded) || got.IsValid() || time.Since(start) != budget {
			t.Fatalf("DNS completed after %v with mapped = %v, error = %v", time.Since(start), got, err)
		}
	})
}

func TestHostSTUNSingleLocalServer(t *testing.T) {
	for _, test := range []struct {
		name, mapped  string
		allowLoopback bool
		wantSuccess   bool
	}{
		{"private mapping", "10.0.0.9:3000", false, true},
		{"loopback in production", "127.0.0.1:3000", false, false},
		{"loopback enabled", "127.0.0.1:3000", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				local := netip.MustParseAddrPort("127.0.0.1:3000")
				server := iceconfig.STUNServer{Host: "127.0.0.1", Port: 3478}
				mapped := netip.MustParseAddrPort(test.mapped)
				probes := 0
				probe := stubHostBinding(func(_ context.Context, _, target netip.AddrPort, timeout time.Duration) (netip.AddrPort, error) {
					probes++
					if timeout != hostSTUNTimeout || target != netip.MustParseAddrPort("127.0.0.1:3478") {
						t.Fatalf("probed %v for %v", target, timeout)
					}
					return mapped, nil
				})
				got, err := discoverHostSTUN(t.Context(), probe, local, server, test.allowLoopback)
				if probes != 1 || (err == nil) != test.wantSuccess || (test.wantSuccess && got != mapped) {
					t.Fatalf("probes = %d, mapped = %v, error = %v", probes, got, err)
				}
			})
		})
	}
}

func TestHostSTUNBoundsDNSFallback(t *testing.T) {
	base := netip.MustParseAddr("2001:db8::1")
	var answers []netip.Addr
	for ip := base; len(answers) <= maxHostSTUNAddresses; ip = ip.Next() {
		answers = append(answers, ip)
	}
	if got := hostSTUNAddresses(answers, base); !slices.Equal(got, answers[:maxHostSTUNAddresses]) {
		t.Fatalf("selected %v from %v", got, answers)
	}
}
