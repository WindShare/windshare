package networkprobe

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/account"
	"github.com/windshare/windshare/engine/builtinnetwork"
	"github.com/windshare/windshare/engine/networkconfig"
)

func TestAuthorityOriginValidation(t *testing.T) {
	t.Parallel()
	valid := []struct {
		raw, want string
	}{
		{"https://api.ourworld.top", "https://api.ourworld.top"},
		{"https://authority.windshare.cc/", "https://authority.windshare.cc"},
		{"https://staging-authority.example.workers.dev/", "https://staging-authority.example.workers.dev"},
		{"https://127.0.0.1:8443", "https://127.0.0.1:8443"},
		{"https://[::1]:8443/", "https://[::1]:8443"},
	}
	for _, tc := range valid {
		got, err := AuthorityOrigin(tc.raw)
		if err != nil || got != tc.want {
			t.Fatalf("AuthorityOrigin(%q) = (%q, %v), want (%q, nil)", tc.raw, got, err, tc.want)
		}
	}
	invalid := []string{
		"",
		" https://api.example",
		"https://api.example ",
		"http://api.example",
		"http://127.0.0.1:8080",
		"https://user:pass@api.example",
		"https://api.example/v1",
		"https://api.example//",
		"https://api.example?foo=bar",
		"https://api.example?",
		"https://api.example#frag",
		"https://api.example#",
		"https://api.example\\",
		"https:api.example",
		"https://",
		"https://:443",
		"https://api.example:",
		"https://[::1]:",
		"https://api.example:0",
		"https://api.example:70000",
	}
	for _, raw := range invalid {
		if got, err := AuthorityOrigin(raw); err == nil {
			t.Fatalf("AuthorityOrigin(%q) = %q, want error", raw, got)
		}
	}
}

func signedTestReleaseJSON(t *testing.T, network, linkHost, authorityURL string) []byte {
	t.Helper()
	b64 := base64.RawURLEncoding
	rootA := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, ed25519.SeedSize))
	rootB := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{12}, ed25519.SeedSize))
	online := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{13}, ed25519.SeedSize))
	authRoot := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{14}, ed25519.SeedSize))
	now := time.Now().UTC().Truncate(time.Second)
	roots := []nodedirectory.Root{
		{ID: "root-a", Key: rootA.Public().(ed25519.PublicKey)},
		{ID: "root-b", Key: rootB.Public().(ed25519.PublicKey)},
	}
	cert, err := nodedirectory.Certify(nodedirectory.Certificate{
		Format:     nodedirectory.Format,
		Network:    network,
		Generation: 1,
		Key:        b64.EncodeToString(online.Public().(ed25519.PublicKey)),
		NotBefore:  now.Add(-time.Minute),
		ExpiresAt:  now.Add(24 * time.Hour),
	}, "root-a", rootA)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := nodedirectory.Sign(nodedirectory.Payload{
		Format:     nodedirectory.Format,
		Network:    network,
		Generation: 1,
		Serial:     1,
		IssuedAt:   now,
		ExpiresAt:  now.Add(24 * time.Hour),
	}, cert, online, roots)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"network":   network,
		"link_host": linkHost,
		"roots": []map[string]string{
			{"id": "root-a", "key": b64.EncodeToString(rootA.Public().(ed25519.PublicKey))},
			{"id": "root-b", "key": b64.EncodeToString(rootB.Public().(ed25519.PublicKey))},
		},
		"snapshot": json.RawMessage(snapshot),
	}
	if authorityURL != "" {
		manifest["authority"] = map[string]string{
			"url":  authorityURL,
			"root": b64.EncodeToString(authRoot.Public().(ed25519.PublicKey)),
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLoadReleaseAndResolveAuthority(t *testing.T) {
	t.Parallel()
	embedded, err := builtinnetwork.Config("")
	if err != nil {
		t.Fatal(err)
	}
	gotEmbedded, err := LoadRelease("")
	if err != nil {
		t.Fatal(err)
	}
	if gotEmbedded.LinkHost != embedded.LinkHost || gotEmbedded.Directory.Network != embedded.Directory.Network {
		t.Fatalf("empty path did not load embedded release: got %+v, want %+v", gotEmbedded, embedded)
	}

	dir := t.TempDir()
	customPath := filepath.Join(dir, "staging-release.json")
	if err := os.WriteFile(customPath, signedTestReleaseJSON(t, "windshare-staging", "https://staging-front.example", "https://staging-api.example/"), 0o644); err != nil {
		t.Fatal(err)
	}
	custom, err := LoadRelease(customPath)
	if err != nil {
		t.Fatal(err)
	}
	if custom.Directory.Network != "windshare-staging" || custom.LinkHost != "https://staging-front.example" || len(custom.Directory.Roots) != 2 {
		t.Fatalf("unexpected custom release config: %+v", custom)
	}
	resolved, err := ResolveAuthority(custom, "")
	if err != nil || resolved != "https://staging-api.example" {
		t.Fatalf("ResolveAuthority(custom, empty) = (%q, %v)", resolved, err)
	}
	overridden, err := ResolveAuthority(custom, "https://override.workers.dev/")
	if err != nil || overridden != "https://override.workers.dev" {
		t.Fatalf("ResolveAuthority(custom, override) = (%q, %v)", overridden, err)
	}
	if _, err := ResolveAuthority(&networkconfig.Config{}, ""); err == nil {
		t.Fatal("missing release authority accepted")
	}
	if got, err := ResolveAuthority(&networkconfig.Config{}, "https://override.example"); err != nil || got != "https://override.example" {
		t.Fatalf("ResolveAuthority(nil authority, override) = (%q, %v)", got, err)
	}
	if _, err := ResolveAuthority(&networkconfig.Config{Authority: &account.Config{URL: "http://insecure.example"}}, ""); err == nil {
		t.Fatal("insecure release authority accepted")
	}

	oversizedPath := filepath.Join(dir, "oversized.json")
	if err := os.WriteFile(oversizedPath, bytes.Repeat([]byte{' '}, maxReleaseManifestBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRelease(oversizedPath); err == nil {
		t.Fatal("oversized release manifest accepted")
	}
	if _, err := LoadRelease(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing release manifest accepted")
	}
}
