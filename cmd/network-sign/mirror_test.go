package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/networkdirectory"
)

func TestMirrorReconcilesTheInstalledReleaseDuringAnUpstreamOutage(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "network", "directory.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Network string
		Now     time.Time
		Roots   []struct{ ID, Key string }
		Entries []struct {
			Name     string
			Envelope json.RawMessage
		}
	}
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	key, err := base64.RawURLEncoding.DecodeString(fixtures.Roots[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	vector := map[string][]byte{}
	for _, v := range fixtures.Entries {
		vector[v.Name] = v.Envelope
	}
	config := networkdirectory.Config{Network: fixtures.Network, Roots: []nodedirectory.Root{{ID: fixtures.Roots[0].ID, Key: ed25519.PublicKey(key)}}, Embedded: vector["older_serial"]}
	dir := t.TempDir()
	out, bundle := filepath.Join(dir, "mirror"), filepath.Join(dir, "bundle")
	if err := os.WriteFile(out, vector["valid"], 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundle, vector["new_generation"], 0644); err != nil {
		t.Fatal(err)
	}
	if err = reconcileMirror(config, out, bundle, nil, fixtures.Now); err != nil {
		t.Fatal(err)
	}
	if err = reconcileMirror(config, out, bundle, vector["older_serial"], fixtures.Now); !errors.Is(err, nodedirectory.ErrRollback) {
		t.Fatal(err)
	}
	result, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := nodedirectory.Verify(result, config.Network, config.Roots)
	if err != nil || snap.Cursor().Generation != 2 {
		t.Fatalf("unexpected mirror %v, %v", snap.Cursor(), err)
	}
	expired := snap.TrustedUntil(nodedirectory.DefaultGrace)
	if err = reconcileMirror(config, out, bundle, nil, expired); !errors.Is(err, nodedirectory.ErrExpired) {
		t.Fatalf("expiry returned %v", err)
	}
	if _, err = os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired public file: %v", err)
	}
	retired, err := os.ReadFile(out + ".expired")
	if err != nil || string(retired) != string(result) {
		t.Fatalf("lost the accepted cursor on expiry: %v", err)
	}
	if err = reconcileMirror(config, out, "", vector["older_serial"], fixtures.Now); !errors.Is(err, nodedirectory.ErrRollback) {
		t.Fatalf("rollback after a restart: %v", err)
	}
	if _, err = os.Stat(out + ".expired"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retirement still hides a usable revision: %v", err)
	}
}
