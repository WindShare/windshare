package builtinnetwork

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReleasePinsTrustAndLeavesUnprovisionedBuildsExplicit(t *testing.T) {
	config, err := Parse([]byte(`{"network":"test","link_host":"https://front.example","mirrors":["https://mirror.example"],"native_fallback_mirrors":["https://release-asset.example/directory.json"],"roots":[],"snapshot":null}`), filepath.Join(t.TempDir(), "config.json"))
	if err != nil || config.LinkHost != "https://front.example" || len(config.Directory.Mirrors) != 0 || len(config.Directory.FallbackMirrors) != 0 || config.Directory.Cache == nil {
		t.Fatal(config, err)
	}
	data, err := os.ReadFile("../../testdata/network/directory.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Roots   []struct{ ID, Key string }
		Entries []struct{ Envelope json.RawMessage }
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var release releaseConfig
	release.Network = "test"
	release.Snapshot = fixture.Entries[0].Envelope
	release.Roots = append(release.Roots, struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}{fixture.Roots[0].ID, fixture.Roots[0].Key})
	encode := func() []byte {
		data, err := json.Marshal(release)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if _, err := Parse(encode(), ""); err == nil {
		t.Fatal("single built-in root accepted")
	}
	spare := release.Roots[0]
	spare.ID, spare.Key = "spare", base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	release.Roots = append(release.Roots, spare)
	release.Mirrors = []string{"https://live.example/.well-known/windshare-network"}
	release.NativeFallbackMirrors = []string{"https://cold.example/windshare-network.json"}
	provisioned, err := Parse(encode(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(provisioned.Directory.Mirrors, release.Mirrors) || !slices.Equal(provisioned.Directory.FallbackMirrors, release.NativeFallbackMirrors) {
		t.Fatalf("mirror tiers = live:%v fallback:%v", provisioned.Directory.Mirrors, provisioned.Directory.FallbackMirrors)
	}
	release.Roots[1] = release.Roots[0]
	if _, err := Parse(encode(), ""); err == nil {
		t.Fatal("duplicate roots accepted")
	}
	release.Roots[1] = spare
	release.Roots[1].Key = "bad"
	if _, err := Parse(encode(), ""); err == nil {
		t.Fatal("malformed root accepted")
	}
	release.Roots = nil
	if _, err := Parse(encode(), ""); err == nil {
		t.Fatal("snapshot without root accepted")
	}
	if _, err := Parse([]byte("{"), ""); err == nil {
		t.Fatal("malformed release accepted")
	}
}

func TestEmbeddedReleaseSeparatesLiveAndNativeFallbackMirrors(t *testing.T) {
	config, err := Config("")
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Directory.Mirrors) < 2 || len(config.Directory.FallbackMirrors) == 0 {
		t.Fatalf("unexpected mirror tiers: live=%v fallback=%v", config.Directory.Mirrors, config.Directory.FallbackMirrors)
	}
	for _, mirror := range config.Directory.Mirrors {
		if strings.Contains(strings.ToLower(mirror), "github.com") {
			t.Fatalf("live mirror list contains release asset %q", mirror)
		}
	}
}
