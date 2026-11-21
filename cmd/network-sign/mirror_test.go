package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/networkdirectory"
)

func loadMirrorFixtures(t *testing.T) (networkdirectory.Config, map[string][]byte, time.Time) {
	t.Helper()
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
	config := networkdirectory.Config{
		Network:  fixtures.Network,
		Roots:    []nodedirectory.Root{{ID: fixtures.Roots[0].ID, Key: ed25519.PublicKey(key)}},
		Embedded: vector["older_serial"],
	}
	return config, vector, fixtures.Now
}

func TestMirrorReconcilesTheInstalledReleaseDuringAnUpstreamOutage(t *testing.T) {
	config, vector, now := loadMirrorFixtures(t)
	dir := t.TempDir()
	out, bundle := filepath.Join(dir, "mirror"), filepath.Join(dir, "bundle")
	if err := os.WriteFile(out, vector["valid"], 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundle, vector["new_generation"], 0644); err != nil {
		t.Fatal(err)
	}
	if err := reconcileMirror(config, out, bundle, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := reconcileMirror(config, out, bundle, vector["older_serial"], now); !errors.Is(err, nodedirectory.ErrRollback) {
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
	if err = reconcileMirror(config, out, "", vector["older_serial"], now); !errors.Is(err, nodedirectory.ErrRollback) {
		t.Fatalf("rollback after a restart: %v", err)
	}
	if _, err = os.Stat(out + ".expired"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retirement still hides a usable revision: %v", err)
	}
}

func TestReadInstalledMirrorRejectsOversizedAndNonRegularInputs(t *testing.T) {
	config, vector, now := loadMirrorFixtures(t)
	dir := t.TempDir()

	oversized := filepath.Join(dir, "oversized.json")
	if err := os.WriteFile(oversized, bytes.Repeat([]byte{' '}, nodedirectory.MaximumEnvelopeBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readInstalledMirror(config, oversized, ""); !errors.Is(err, nodedirectory.ErrFormat) {
		t.Fatalf("oversized mirror err = %v, want %v", err, nodedirectory.ErrFormat)
	}

	asDir := filepath.Join(dir, "dir.json")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readInstalledMirror(config, asDir, ""); !errors.Is(err, errNonRegularFile) {
		t.Fatalf("directory mirror err = %v, want %v", err, errNonRegularFile)
	}

	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, vector["valid"], 0o644); err != nil {
		t.Fatal(err)
	}
	leafLink := filepath.Join(dir, "leaf-link.json")
	if err := os.Symlink(target, leafLink); err == nil {
		if _, err := readInstalledMirror(config, leafLink, ""); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("leaf symlink mirror err = %v, want rejection", err)
		}
	}

	// The timer resolves --bundle through /srv/windshare/current, so intermediate directory symlinks must still work.
	releaseDir := filepath.Join(dir, "releases", "rev1", "web", ".well-known")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, "windshare-network"), vector["new_generation"], 0o644); err != nil {
		t.Fatal(err)
	}
	currentLink := filepath.Join(dir, "current")
	if err := os.Symlink(filepath.Join(dir, "releases", "rev1"), currentLink); err == nil {
		out := filepath.Join(dir, "published.json")
		bundle := filepath.Join(currentLink, "web", ".well-known", "windshare-network")
		if err := reconcileMirror(config, out, bundle, nil, now); err != nil {
			t.Fatalf("intermediate directory symlink bundle failed: %v", err)
		}
		published, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		snap, err := nodedirectory.Verify(published, config.Network, config.Roots)
		if err != nil || snap.Cursor().Generation != 2 {
			t.Fatalf("unexpected mirror from symlinked release dir: %v, %v", snap.Cursor(), err)
		}
	}
}

func TestMirrorSyncsParentDirectoryAcrossPublishAndWithdrawTransitions(t *testing.T) {
	config, vector, now := loadMirrorFixtures(t)
	newGen, err := nodedirectory.Verify(vector["new_generation"], config.Network, config.Roots)
	if err != nil {
		t.Fatal(err)
	}
	expired := newGen.TrustedUntil(nodedirectory.DefaultGrace)

	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}
	type syncEvent struct {
		path           string
		outPresent     bool
		retiredPresent bool
	}

	hookSync := func(t *testing.T, fn func(string) error) {
		t.Helper()
		previous := syncParentDirectory
		syncParentDirectory = fn
		t.Cleanup(func() { syncParentDirectory = previous })
	}

	t.Run("withdraw syncs retired marker before removing public copy", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "windshare-network.json")
		retired := out + ".expired"
		if err := os.WriteFile(out, vector["new_generation"], 0o644); err != nil {
			t.Fatal(err)
		}
		var events []syncEvent
		hookSync(t, func(path string) error {
			events = append(events, syncEvent{
				path:           path,
				outPresent:     exists(out),
				retiredPresent: exists(retired),
			})
			return nil
		})
		if err := reconcileMirror(config, out, "", nil, expired); !errors.Is(err, nodedirectory.ErrExpired) {
			t.Fatalf("withdraw err = %v, want %v", err, nodedirectory.ErrExpired)
		}
		want := []syncEvent{
			{path: retired, outPresent: true, retiredPresent: true},
			{path: out, outPresent: false, retiredPresent: true},
		}
		if !slices.Equal(events, want) {
			t.Fatalf("withdraw sync events = %+v, want %+v", events, want)
		}
	})

	t.Run("withdraw aborts before removing public copy if retired sync fails", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "windshare-network.json")
		if err := os.WriteFile(out, vector["new_generation"], 0o644); err != nil {
			t.Fatal(err)
		}
		syncErr := errors.New("parent dir sync failed")
		hookSync(t, func(string) error { return syncErr })
		if err := reconcileMirror(config, out, "", nil, expired); !errors.Is(err, syncErr) {
			t.Fatalf("withdraw err = %v, want %v", err, syncErr)
		}
		if !exists(out) {
			t.Fatal("public copy was removed even though retired marker directory sync failed")
		}
	})

	t.Run("publish syncs new public copy before removing retired marker", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "windshare-network.json")
		retired := out + ".expired"
		if err := os.WriteFile(retired, vector["valid"], 0o644); err != nil {
			t.Fatal(err)
		}
		var events []syncEvent
		hookSync(t, func(path string) error {
			events = append(events, syncEvent{
				path:           path,
				outPresent:     exists(out),
				retiredPresent: exists(retired),
			})
			return nil
		})
		if err := reconcileMirror(config, out, "", vector["new_generation"], now); err != nil {
			t.Fatalf("publish err = %v", err)
		}
		want := []syncEvent{
			{path: out, outPresent: true, retiredPresent: true},
			{path: retired, outPresent: true, retiredPresent: false},
		}
		if !slices.Equal(events, want) {
			t.Fatalf("publish sync events = %+v, want %+v", events, want)
		}
	})

	t.Run("publish aborts before removing retired marker if public sync fails", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "windshare-network.json")
		retired := out + ".expired"
		if err := os.WriteFile(retired, vector["valid"], 0o644); err != nil {
			t.Fatal(err)
		}
		syncErr := errors.New("parent dir sync failed")
		hookSync(t, func(string) error { return syncErr })
		if err := reconcileMirror(config, out, "", vector["new_generation"], now); !errors.Is(err, syncErr) {
			t.Fatalf("publish err = %v, want %v", err, syncErr)
		}
		if !exists(retired) {
			t.Fatal("retired marker was removed even though public copy directory sync failed")
		}
	})
}
