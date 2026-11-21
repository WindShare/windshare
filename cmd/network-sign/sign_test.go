package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
)

func TestOfflineAuthorityAndImmutableDirectoryHistory(t *testing.T) {
	dir := t.TempDir()
	path := func(name string) string { return filepath.Join(dir, name) }
	var roots []publicKey
	for _, id := range []string{"root", "spare", "online"} {
		if err := run([]string{"keygen", "--id", id, "--private", path(id + ".pem"), "--public", path(id + ".json")}); err != nil {
			t.Fatal(err)
		}
		if id != "online" {
			var public publicKey
			if err := readJSON(path(id+".json"), &public); err != nil {
				t.Fatal(err)
			}
			roots = append(roots, public)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := run([]string{"authorize", "--root-private", path("root.pem"), "--root-id", "root", "--online-public", path("online.json"),
		"--generation", "1", "--not-before", now.Add(-time.Hour).Format(time.RFC3339), "--expires", now.Add(48 * time.Hour).Format(time.RFC3339), "--out", path("certificate.json")}); err != nil {
		t.Fatal(err)
	}
	authority := json.RawMessage(`{"url":"https://api.example","roots":["account-root"]}`)
	manifest := releaseManifest{
		Network:               "windshare",
		Roots:                 roots,
		Snapshot:              json.RawMessage("null"),
		LinkHost:              "https://front.example",
		Mirrors:               []string{"https://front.example/.well-known/windshare-network"},
		NativeFallbackMirrors: []string{"https://release-asset.example/windshare-network.json"},
		Authority:             authority,
	}
	if err := writeJSON(path("release.json"), manifest); err != nil {
		t.Fatal(err)
	}
	payload := nodedirectory.Payload{Format: 1, Network: "windshare", Generation: 1, Serial: 2, IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour),
		Nodes: []nodedirectory.Node{{Alias: "a", NodeKey: roots[0].Key, Relay: "https://a.example", Roles: []string{"relay"}, Zone: "global", Region: "jp", Provider: "p", Operator: "o", CapacityMbps: 50, Status: nodedirectory.Active}}}
	arguments := []string{"export", "--source", path("signed.json"), "--release", path("release.json"), "--history", path("history"),
		"--payload", path("source.json"), "--out", path("mirror.json")}
	issue := func() error {
		var cert nodedirectory.SignedCertificate
		if err := readJSON(path("certificate.json"), &cert); err != nil {
			return err
		}
		key, err := privateKey(path("online.pem"))
		if err != nil {
			return err
		}
		roots, err := releaseRoots(manifest)
		if err != nil {
			return err
		}
		issued, err := nodedirectory.Sign(payload, cert, key, roots)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path("signed.json"), issued, 0600); err != nil {
			t.Fatal(err)
		}
		return run(arguments)
	}
	if err := issue(); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path("mirror.json"))
	if err := issue(); err != nil {
		t.Fatal("repeat must be deterministic", err)
	}
	var updated releaseManifest
	if err := readJSON(path("release.json"), &updated); err != nil {
		t.Fatal(err)
	}
	var preserved bytes.Buffer
	if err := json.Compact(&preserved, updated.Authority); err != nil || !bytes.Equal(preserved.Bytes(), authority) {
		t.Fatalf("directory signing replaced account authority: %s, %v", updated.Authority, err)
	}
	if !slices.Equal(updated.Mirrors, manifest.Mirrors) || !slices.Equal(updated.NativeFallbackMirrors, manifest.NativeFallbackMirrors) {
		t.Fatalf("directory signing altered mirrors: live=%v fallback=%v", updated.Mirrors, updated.NativeFallbackMirrors)
	}
	var embedded, exported any
	_ = json.Unmarshal(updated.Snapshot, &embedded)
	_ = json.Unmarshal(first, &exported)
	a, _ := json.Marshal(embedded)
	b, _ := json.Marshal(exported)
	if !bytes.Equal(a, b) {
		t.Fatal("release/mirror mismatch")
	}
	verification := []string{"verify", "--release", path("release.json"), "--mirror", path("mirror.json"), "--history", path("history")}
	if err := run(verification); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path("mirror.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(verification); err == nil {
		t.Fatal("mismatched mirror accepted")
	}
	if err := os.WriteFile(path("mirror.json"), first, 0o644); err != nil {
		t.Fatal(err)
	}
	payload.Serial = 1
	if err := issue(); err == nil {
		t.Fatal("serial rollback signed")
	}
	payload.Serial = 3
	payload.Nodes = nil
	if err := issue(); err != nil {
		t.Fatal(err)
	}
	if err := run(verification); err != nil {
		t.Fatal(err)
	}
	payload.Serial = 4
	payload.Nodes = []nodedirectory.Node{{Alias: "a", NodeKey: roots[0].Key, Relay: "https://replacement.example", Roles: []string{"relay"}, Zone: "global", Region: "jp", Provider: "p", Operator: "o", CapacityMbps: 50, Status: nodedirectory.Active}}
	if err := issue(); err == nil {
		t.Fatal("removed alias reassigned")
	}
	before, _ := os.ReadFile(path("online.pem"))
	if err := run([]string{"keygen", "--id", "overwrite", "--private", path("online.pem"), "--public", path("overwrite.json")}); err == nil {
		t.Fatal("private key overwritten")
	}
	after, _ := os.ReadFile(path("online.pem"))
	if !bytes.Equal(before, after) {
		t.Fatal("private key changed")
	}
}

func TestSigningPathsAndMalformedInputs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	if err := distinctPaths(path, filepath.Join(dir, ".", "key.pem")); err == nil {
		t.Fatal("same file accepted")
	}
	if err := distinctPaths("", path); err == nil {
		t.Fatal("empty path accepted")
	}
	for _, data := range []string{"{} {}", `{"unknown":1}`, string(bytes.Repeat([]byte{' '}, nodedirectory.MaximumEnvelopeBytes+1))} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := readJSON(path, &publicKey{}); err == nil {
			t.Fatal("malformed input accepted")
		}
	}
	if _, err := privateKey(path); err == nil {
		t.Fatal("invalid PEM accepted")
	}
	for _, args := range [][]string{nil, {"unknown"}, {"keygen"}, {"sign", "extra"}, {"authorize"}} {
		if err := run(args); err == nil {
			t.Fatal(args)
		}
	}
}

func TestExportGuardsSourcePathSizeAndAuthorityURL(t *testing.T) {
	dir := t.TempDir()
	path := func(name string) string { return filepath.Join(dir, name) }
	var roots []publicKey
	for _, id := range []string{"root", "spare", "online"} {
		if err := run([]string{"keygen", "--id", id, "--private", path(id + ".pem"), "--public", path(id + ".json")}); err != nil {
			t.Fatal(err)
		}
		if id != "online" {
			var public publicKey
			if err := readJSON(path(id+".json"), &public); err != nil {
				t.Fatal(err)
			}
			roots = append(roots, public)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := run([]string{"authorize", "--root-private", path("root.pem"), "--root-id", "root", "--online-public", path("online.json"),
		"--generation", "1", "--not-before", now.Add(-time.Hour).Format(time.RFC3339), "--expires", now.Add(48 * time.Hour).Format(time.RFC3339), "--out", path("certificate.json")}); err != nil {
		t.Fatal(err)
	}
	manifest := releaseManifest{Network: "windshare", Roots: roots, Snapshot: json.RawMessage("null"), LinkHost: "https://front.example"}
	if err := writeJSON(path("release.json"), manifest); err != nil {
		t.Fatal(err)
	}
	var cert nodedirectory.SignedCertificate
	if err := readJSON(path("certificate.json"), &cert); err != nil {
		t.Fatal(err)
	}
	key, err := privateKey(path("online.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := releaseRoots(manifest)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := nodedirectory.Sign(nodedirectory.Payload{Format: 1, Network: "windshare", Generation: 1, Serial: 1, IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour)}, cert, key, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path("signed.json"), signed, 0o600); err != nil {
		t.Fatal(err)
	}
	// Reject overwriting the input envelope when --source matches an output path.
	if err := run([]string{"export", "--source", path("signed.json"), "--release", path("release.json"), "--history", path("history"),
		"--payload", path("signed.json"), "--out", path("mirror.json")}); err == nil {
		t.Fatal("colliding --source and --payload accepted")
	}
	after, err := os.ReadFile(path("signed.json"))
	if err != nil || !bytes.Equal(after, signed) {
		t.Fatal("colliding export modified --source")
	}
	if err := os.WriteFile(path("oversized.json"), bytes.Repeat([]byte{' '}, nodedirectory.MaximumEnvelopeBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"export", "--source", path("oversized.json"), "--release", path("release.json"), "--history", path("history"),
		"--payload", path("payload.json"), "--out", path("mirror.json")}); !errors.Is(err, nodedirectory.ErrFormat) {
		t.Fatalf("oversized --source err = %v, want %v", err, nodedirectory.ErrFormat)
	}
	for _, authority := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage(`{}`), json.RawMessage(`{"url":""}`)} {
		manifest.Authority = authority
		if err := writeJSON(path("release.json"), manifest); err != nil {
			t.Fatal(err)
		}
		err := run([]string{"export", "--release", path("release.json"), "--history", path("history"),
			"--payload", path("payload.json"), "--out", path("mirror.json")})
		if err == nil || !strings.Contains(err.Error(), "release has no public authority") {
			t.Fatalf("authority %s err = %v", authority, err)
		}
	}
}
