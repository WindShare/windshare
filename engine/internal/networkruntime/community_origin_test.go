package networkruntime

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

// A frontend saved or overridden with a Unicode or non-canonical host/port must
// dial community relays with the canonical browser ASCII Origin; relays refuse
// non-ASCII Origin values with HTTP 403, and non-canonical spellings would split
// community verification scopes.
func TestCommunityRouteOriginUsesCanonicalBrowserSerialization(t *testing.T) {
	for _, test := range []struct {
		frontend string
		want     string
	}{
		{"https://文件.example/share/", "https://xn--5nqy36c.example"},
		{"https://文件.EXAMPLE:443/share/", "https://xn--5nqy36c.example"},
		{"https://WS.EXAMPLE:443/share/", "https://ws.example"},
	} {
		t.Run(test.frontend, func(t *testing.T) {
			r := fixtureRuntime(t)
			writeCachedCommunityPool(t, r, cachedCommunityPeer{host: "cached.example", seedByte: 43})
			r.directory.Close()
			r.directory = nil
			selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), test.frontend)
			if err != nil || len(selection.Relays) != 1 {
				t.Fatal(selection, err)
			}
			reg := selection.Relays[0]
			if selection.Plan.Network[0].Source != relayplan.Community || reg.Community == nil {
				t.Fatal(reg, selection.Plan)
			}
			if reg.Community.Origin != test.want {
				t.Fatalf("community Origin = %q, want %q", reg.Community.Origin, test.want)
			}
		})
	}
}

func TestFrontendOriginMatchesBrowserSerialization(t *testing.T) {
	for frontend, want := range map[string]string{
		"https://ws.example/":              "https://ws.example",
		"https://WS.EXAMPLE:443/app":       "https://ws.example",
		"http://WS.Example:080/app":        "http://ws.example",
		"https://ws.example:80/app":        "https://ws.example:80",
		"http://192.0.2.1:8080/app":        "http://192.0.2.1:8080",
		"https://[2001:0db8::0001]:443/":   "https://[2001:db8::1]",
		"https://[2001:db8::1]:8443/":      "https://[2001:db8::1]:8443",
		"https://文件.example/share/":        "https://xn--5nqy36c.example",
		"https://XN--5NQY36C.EXAMPLE:443/": "https://xn--5nqy36c.example",
		"https://bücher.example:8443/x":    "https://xn--bcher-kva.example:8443",
		"https://ex。com/":                  "https://ex.com",
		"https://文件.example./":             "https://xn--5nqy36c.example.",
		"https://xn--test.example/":        "",
		"https://ws.example:0/":            "",
		"wss://ws.example/":                "",
		"ftp://ws.example/":                "",
		"":                                 "",
	} {
		if got := frontendOrigin(frontend); got != want {
			t.Errorf("frontendOrigin(%q) = %q, want %q", frontend, got, want)
		}
	}
}

func TestEquivalentFrontendSpellingsShareScopeAndDeduplication(t *testing.T) {
	r := fixtureRuntime(t)
	settingsPath := writeCachedCommunityPool(t, r, cachedCommunityPeer{host: "cached.example", seedByte: 43})
	r.directory.Close()
	r.directory = nil
	r.communityKick = make(chan struct{}, 1)

	store := relayconfig.FileStore{Path: settingsPath}
	if err := store.Save(relayconfig.Config{
		Profiles: []relayprofile.Candidate{},
		FrontURL: "https://文件.example/share/",
	}); err != nil {
		t.Fatal(err)
	}

	// Equivalent per-share spellings of the saved Unicode frontend must take the
	// clearExplicitOrigin path instead of latching r.communityOrigin or re-warming.
	for _, override := range []string{
		"https://xn--5nqy36c.example/share/",
		"https://XN--5NQY36C.EXAMPLE:443/other",
		"https://文件.EXAMPLE:443/",
	} {
		if _, err := r.Plan(t.Context(), relayprofile.Plan(nil), override); err != nil {
			t.Fatalf("Plan(%q) error = %v", override, err)
		}
		r.mu.Lock()
		latched := r.communityOrigin
		r.mu.Unlock()
		if latched != "" {
			t.Fatalf("Plan(%q) latched communityOrigin = %q, want empty", override, latched)
		}
		if len(r.communityKick) != 0 {
			t.Fatalf("Plan(%q) triggered spurious community re-warm", override)
		}
	}

	if seedOriginKey("https://文件.EXAMPLE:443/") != seedOriginKey("https://xn--5nqy36c.example") {
		t.Fatalf("seedOriginKey split Unicode and Punycode spellings: %q vs %q",
			seedOriginKey("https://文件.EXAMPLE:443/"), seedOriginKey("https://xn--5nqy36c.example"))
	}
	if !sameFrontendBase("https://文件.example/app/", "https://XN--5NQY36C.EXAMPLE:443/app") {
		t.Fatal("sameFrontendBase did not match canonicalized Unicode and Punycode frontends")
	}
}

func TestPlanRejectsInvalidFrontendBeforeLatchingCommunityOrigin(t *testing.T) {
	r := fixtureRuntime(t)
	writeCachedCommunityPool(t, r, cachedCommunityPeer{host: "cached.example", seedByte: 43})
	r.directory.Close()
	r.directory = nil
	r.communityKick = make(chan struct{}, 1)

	for _, invalid := range []string{
		"ftp://bad.example/",
		"https://user:pass@ws.example/",
		"https://ws.example:0/",
		"https://xn--test.example/",
	} {
		if _, err := r.Plan(t.Context(), relayprofile.Plan(nil), invalid); !errors.Is(err, relayconfig.ErrFrontURL) {
			t.Fatalf("Plan(%q) err = %v, want ErrFrontURL", invalid, err)
		}
		r.mu.Lock()
		latched := r.communityOrigin
		r.mu.Unlock()
		if latched != "" {
			t.Fatalf("invalid Plan(%q) latched communityOrigin = %q", invalid, latched)
		}
		if len(r.communityKick) != 0 {
			t.Fatalf("invalid Plan(%q) kicked background community refresh", invalid)
		}
	}
}
