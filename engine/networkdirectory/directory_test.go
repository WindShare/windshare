package networkdirectory

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
)

type signingFixture struct {
	root, online ed25519.PrivateKey
	roots        []nodedirectory.Root
	now          time.Time
}

func signing() signingFixture {
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32))
	online := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, 32))
	return signingFixture{root: root, online: online, roots: []nodedirectory.Root{{ID: "root", Key: root.Public().(ed25519.PublicKey)}}, now: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)}
}
func (f signingFixture) envelope(t *testing.T, generation, serial uint64, age time.Duration) []byte {
	t.Helper()
	certificate, err := nodedirectory.Certify(nodedirectory.Certificate{Format: 1, Network: "fixture", Generation: generation,
		Key: base64.RawURLEncoding.EncodeToString(f.online.Public().(ed25519.PublicKey)), NotBefore: f.now.Add(-age - time.Hour), ExpiresAt: f.now.Add(30 * 24 * time.Hour)}, "root", f.root)
	if err != nil {
		t.Fatal(err)
	}
	payload := nodedirectory.Payload{Format: 1, Network: "fixture", Generation: generation, Serial: serial, IssuedAt: f.now.Add(-age), ExpiresAt: f.now.Add(-age + 24*time.Hour),
		Nodes: []nodedirectory.Node{{Alias: "jp1", Relay: "https://jp1.example", Roles: []string{"relay"}, Zone: "global", Region: "jp", Provider: "provider", Operator: "operator", CapacityMbps: 50, Status: nodedirectory.Active}}}
	data, err := nodedirectory.Sign(payload, certificate, f.online, f.roots)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func (f signingFixture) config() Config {
	return Config{Network: "fixture", Roots: f.roots, Now: func() time.Time { return f.now }, Order: func(n int) []int {
		indices := make([]int, n)
		for i := range indices {
			indices[i] = i
		}
		return indices
	}}
}

func TestCancelledWaiterDoesNotCancelSharedRefresh(t *testing.T) {
	f := signing()
	config := f.config()
	data := f.envelope(t, 1, 1, 0)
	config.Mirrors = []string{"https://mirror.example/network"}
	entered, release := make(chan context.Context, 1), make(chan struct{})
	config.Fetch = func(ctx context.Context, _ string) ([]byte, error) {
		entered <- ctx
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return data, nil
		}
	}
	directory, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	request := <-entered
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := directory.Refresh(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if request.Err() != nil {
		t.Fatal("caller cancelled shared request")
	}
	directory.mu.Lock()
	done := directory.refreshing
	directory.mu.Unlock()
	close(release)
	<-done
	snapshot, usable := directory.Snapshot()
	if !usable || snapshot.Cursor().Serial != 1 {
		t.Fatal("refresh not installed")
	}
}

func TestExpiredReleaseSeedStillPreventsGenerationRollback(t *testing.T) {
	f := signing()
	config := f.config()
	config.Embedded = f.envelope(t, 2, 1, 10*24*time.Hour)
	config.Mirrors = []string{"https://mirror.example/network"}
	old := f.envelope(t, 1, 100, 0)
	config.Fetch = func(context.Context, string) ([]byte, error) { return old, nil }
	directory, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := directory.Refresh(context.Background()); !errors.Is(err, nodedirectory.ErrRollback) {
		t.Fatal(err)
	}
	snapshot, usable := directory.Snapshot()
	if usable || snapshot.Cursor().Generation != 2 {
		t.Fatal("expired trust resurrected")
	}
}

func TestMirrorFallbackBackoffAndAliasRefresh(t *testing.T) {
	f := signing()
	config := f.config()
	config.Embedded = f.envelope(t, 1, 1, 0)
	config.Mirrors = []string{"https://bad.example/network", "https://good.example/network"}
	data := f.envelope(t, 1, 2, 0)
	var calls atomic.Int32
	config.Fetch = func(_ context.Context, address string) ([]byte, error) {
		calls.Add(1)
		if address == config.Mirrors[0] {
			return []byte(`{}`), nil
		}
		return data, nil
	}
	directory, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := directory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, usable := directory.Snapshot()
	if !usable || snapshot.Cursor().Serial != 2 {
		t.Fatal("mirror fallback lost verified snapshot")
	}
	// Success must not suppress an explicit unknown-alias refresh for six hours.
	previous := calls.Load()
	if err := directory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() <= previous {
		t.Fatal("explicit refresh suppressed by periodic schedule")
	}
}

func TestFileCacheSerializesCompetingDirectoryUpdates(t *testing.T) {
	f := signing()
	cache := NewFileCache(filepath.Join(t.TempDir(), "config.json"))
	config := f.config()
	config.Cache = cache
	config.Embedded = f.envelope(t, 1, 10, 0)
	first, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	config.Embedded = f.envelope(t, 1, 9, 0)
	second, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	snapshot, usable := second.Snapshot()
	if !usable || snapshot.Cursor().Serial != 10 {
		t.Fatal("disk rollback")
	}
	record, err := cache.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if record.Cursor.Serial != 10 || record.Aliases["jp1"] != "wss://jp1.example/v2/ws" {
		t.Fatal(record)
	}
}
