package networkdirectory

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
		Nodes: []nodedirectory.Node{{Alias: "jp1", NodeKey: base64.RawURLEncoding.EncodeToString(f.online.Public().(ed25519.PublicKey)), Relay: "https://jp1.example", Roles: []string{"relay"}, Zone: "global", Region: "jp", Provider: "provider", Operator: "operator", CapacityMbps: 50, Status: nodedirectory.Active}}}
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
	path := filepath.Join(t.TempDir(), "config.json")
	cache := NewFileCache(path, f.config().Network)
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
	other, err := NewFileCache(path, "other-network").Load(t.Context())
	if err != nil || len(other.Envelope) != 0 {
		t.Fatal("independent networks shared a trust history", err)
	}
	if record.Cursor.Serial != 10 || record.Aliases["jp1"] != "wss://jp1.example/v2/ws" {
		t.Fatal(record)
	}
}

func TestAutomaticRefreshCoalescesAndRetainsFailureBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := signing()
		config := f.config()
		data := f.envelope(t, 1, 1, 0)
		config.Embedded = data
		config.Mirrors = []string{"https://mirror.example/network"}
		start := time.Now()
		config.Now = func() time.Time { return f.now.Add(time.Since(start)) }
		var calls atomic.Int32
		var offline atomic.Bool
		config.Fetch = func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			if offline.Load() {
				return nil, errors.New("offline")
			}
			return data, nil
		}
		d, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		synctest.Wait()
		trigger := func() {
			for range 10 {
				d.TriggerRefresh()
			}
			synctest.Wait()
		}
		expect := func(n int32) {
			t.Helper()
			synctest.Wait()
			if calls.Load() != n {
				t.Fatalf("refreshes = %d, want %d", calls.Load(), n)
			}
		}
		expect(1)
		trigger()
		time.Sleep(MinimumBackoff - time.Nanosecond)
		expect(1)
		time.Sleep(time.Nanosecond)
		expect(2)
		// An unknown alias can still require an immediate signed directory read.
		if err := d.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		expect(3)
		offline.Store(true)
		if err := d.Refresh(t.Context()); !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
		expect(4)
		trigger()
		time.Sleep(MinimumBackoff)
		expect(5)
		trigger()
		time.Sleep(2*MinimumBackoff - time.Nanosecond)
		expect(5)
		offline.Store(false)
		time.Sleep(time.Nanosecond)
		expect(6)
	})
}

func TestCloseJoinsRefreshDuringConcurrentRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := signing().config()
		config.Mirrors = []string{"https://mirror.example/network"}
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		config.Fetch = func(ctx context.Context, _ string) ([]byte, error) {
			calls.Add(1)
			close(entered)
			<-ctx.Done()
			<-release
			return nil, ctx.Err()
		}
		d, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		<-entered
		var callers sync.WaitGroup
		for range 16 {
			callers.Go(func() { _ = d.Refresh(t.Context()) })
		}
		closed := make(chan struct{})
		go func() { d.Close(); close(closed) }()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("Close returned before the fetch released its resources")
		default:
		}
		close(release)
		<-closed
		callers.Wait()
		if err := d.Refresh(t.Context()); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("post-close refresh=%v calls=%d", err, calls.Load())
		}
	})
}
