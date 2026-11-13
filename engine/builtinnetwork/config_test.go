package builtinnetwork

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReleasePinsTrustAndLeavesUnprovisionedBuildsExplicit(t *testing.T) {
	config, err := parse([]byte(`{"network":"test","link_host":"https://front.example","mirrors":["https://mirror.example"],"roots":[],"snapshot":null}`), filepath.Join(t.TempDir(), "config.json"))
	if err != nil || config.LinkHost != "https://front.example" || len(config.Directory.Mirrors) != 0 || config.Directory.Cache == nil {
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
	if _, err := parse(encode(), ""); err == nil {
		t.Fatal("single built-in root accepted")
	}
	spare := release.Roots[0]
	spare.ID, spare.Key = "spare", base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	release.Roots = append(release.Roots, spare)
	if _, err := parse(encode(), ""); err != nil {
		t.Fatal(err)
	}
	release.Roots[1] = release.Roots[0]
	if _, err := parse(encode(), ""); err == nil {
		t.Fatal("duplicate roots accepted")
	}
	release.Roots[1] = spare
	release.Roots[1].Key = "bad"
	if _, err := parse(encode(), ""); err == nil {
		t.Fatal("malformed root accepted")
	}
	release.Roots = nil
	if _, err := parse(encode(), ""); err == nil {
		t.Fatal("snapshot without root accepted")
	}
	if _, err := parse([]byte("{"), ""); err == nil {
		t.Fatal("malformed release accepted")
	}
}
