package publicdial

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestDialPinsEachLookupBeforeConnecting(t *testing.T) {
	public := netip.MustParseAddr("1.1.1.1")
	answer := public
	var dialed []string
	p := Policy{Lookup: func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{answer}, nil },
		Dial: func(_ context.Context, _ string, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return nil, errors.New("test dial")
		}}
	_, _ = p.dial(t.Context(), "tcp", "relay.example:443")
	answer = netip.MustParseAddr("127.0.0.1")
	_, err := p.dial(t.Context(), "tcp", "relay.example:443")
	if !errors.Is(err, ErrAddress) || len(dialed) != 1 || dialed[0] != "1.1.1.1:443" {
		t.Fatal(dialed, err)
	}
	for _, raw := range []string{"http://1.1.1.1", "https://127.0.0.1", "https://[::ffff:127.0.0.1]", "https://user@example.org", "https://192.0.2.1"} {
		if _, err := p.Origin(raw); !errors.Is(err, ErrAddress) {
			t.Fatal(raw, err)
		}
	}
}
