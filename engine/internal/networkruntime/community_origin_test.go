package networkruntime

import (
	"testing"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/engine/relayprofile"
)

// A frontend saved with a Unicode host must dial community relays with the
// Punycode Origin a browser would send; relays refuse non-ASCII Origin values
// with HTTP 403, which would retire every community route for the share.
func TestCommunityRouteOriginUsesPunycodeForInternationalizedFrontend(t *testing.T) {
	r := fixtureRuntime(t)
	writeCachedCommunityPool(t, r, cachedCommunityPeer{host: "cached.example", seedByte: 43})
	r.directory.Close()
	r.directory = nil
	selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), "https://文件.example/share/")
	if err != nil || len(selection.Relays) != 1 {
		t.Fatal(selection, err)
	}
	reg := selection.Relays[0]
	if selection.Plan.Network[0].Source != relayplan.Community || reg.Community == nil {
		t.Fatal(reg, selection.Plan)
	}
	if want := "https://xn--5nqy36c.example"; reg.Community.Origin != want {
		t.Fatalf("community Origin = %q, want %q", reg.Community.Origin, want)
	}
}

func TestFrontendOriginMatchesBrowserSerializationForASCIIAndUnicodeHosts(t *testing.T) {
	for frontend, want := range map[string]string{
		"https://ws.example/":           "https://ws.example",
		"http://192.0.2.1:8080/app":     "http://192.0.2.1:8080",
		"https://[2001:db8::1]:8443/":   "https://[2001:db8::1]:8443",
		"https://文件.example/share/":     "https://xn--5nqy36c.example",
		"https://bücher.example:8443/x": "https://xn--bcher-kva.example:8443",
		"":                              "",
	} {
		if got := frontendOrigin(frontend); got != want {
			t.Errorf("frontendOrigin(%q) = %q, want %q", frontend, got, want)
		}
	}
}
