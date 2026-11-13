package link_test

import (
	"errors"
	"net/url"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/link"
)

func TestAliasHintsRemainUnresolvedAndPreserveExplicitOrigin(t *testing.T) {
	capability := newLink("https://front.example")
	capability.NodeAliases = []string{"jp1", "sg1"}
	full, err := capability.URL("https://front.example")
	if err != nil {
		t.Fatal(err)
	}
	address, _ := url.Parse(full)
	if got := address.Query().Get("r"); got != "front.example" {
		t.Fatalf("explicit relay omitted or uncompressed: %q", got)
	}
	parsed, err := link.Parse(full)
	if err != nil || !slices.Equal(parsed.Relays, capability.Relays) || !slices.Equal(parsed.NodeAliases, capability.NodeAliases) {
		t.Fatalf("round trip: %+v %v", parsed, err)
	}
	capability.Relays = nil
	full, _ = capability.URL("https://front.example")
	parsed, err = link.Parse(full)
	if err != nil || len(parsed.Relays) != 0 {
		t.Fatalf("alias-only link acquired origin relay: %+v %v", parsed, err)
	}
}

func TestMalformedOrExcessRelayHintsFailBeforeResolution(t *testing.T) {
	for _, query := range []string{"n=", "n=JP1", "n=jp1&n=jp1", "r=", "n=a&n=b&n=c&n=d&n=e&n=f&n=g&n=h&n=i"} {
		if _, err := link.Parse("https://front.example/" + testShareID + "?" + query + "#" + testKey); !errors.Is(err, link.ErrMalformedLink) {
			t.Fatalf("query=%q error=%v", query, err)
		}
	}
}
