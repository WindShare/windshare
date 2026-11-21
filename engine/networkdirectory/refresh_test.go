package networkdirectory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
)

func TestTargetedWaiterContinuesStartupPassAndPreservesAvailability(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := signing()
		stale := f.envelope(t, 1, 1, 0)
		prior, err := nodedirectory.Verify(stale, "fixture", f.roots)
		if err != nil {
			t.Fatal(err)
		}
		updated := prior.Payload()
		updated.Serial++
		updated.Nodes[0].Alias = "new"
		var envelope nodedirectory.Envelope
		if err := json.Unmarshal(stale, &envelope); err != nil {
			t.Fatal(err)
		}
		fresh, err := nodedirectory.Sign(updated, envelope.Certificate, f.online, f.roots)
		if err != nil {
			t.Fatal(err)
		}
		const cold = "https://github.com/releases/directory"
		const primary = "https://ws.ourworld.top/directory"
		const backup = "https://windshare.cc/directory"
		config := f.config()
		config.Mirrors = []string{cold, primary, backup}
		began, unblock := make(chan struct{}), make(chan struct{})
		var calls []string
		config.Fetch = func(_ context.Context, address string) ([]byte, error) {
			calls = append(calls, address)
			if len(calls) == 1 {
				close(began)
				<-unblock
				return stale, nil
			}
			return fresh, nil
		}
		directory, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		defer directory.Close()
		<-began
		done := make(chan error, 1)
		go func() { done <- directory.RefreshAliases(t.Context(), []string{"new"}) }()
		synctest.Wait()
		close(unblock)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(calls, []string{primary, backup}) {
			t.Fatalf("startup used %v", calls)
		}
		if err := directory.RefreshAliases(t.Context(), []string{"nonexistent"}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(calls[2:], []string{primary, backup, cold}) {
			t.Fatalf("targeted pass used %v", calls[2:])
		}
		if err := directory.Refresh(t.Context()); err != nil || calls[len(calls)-1] != primary {
			t.Fatalf("unexpected backoff: %v", err)
		}
	})
}

type countingCache struct {
	inner   Cache
	updates atomic.Int32
}

func (c *countingCache) Load(ctx context.Context) (Record, error) { return c.inner.Load(ctx) }
func (c *countingCache) Update(ctx context.Context, change func(Record) (Record, error)) (Record, error) {
	c.updates.Add(1)
	return c.inner.Update(ctx, change)
}

func TestEqualCursorMirrorsAndMixedCaseReleaseAssetAvoidRedundantWrites(t *testing.T) {
	f := signing()
	snapshot := f.envelope(t, 1, 1, 0)
	fileCache := NewFileCache(filepath.Join(t.TempDir(), "config.json"), f.config().Network)
	cache := &countingCache{inner: fileCache}
	const cold = "https://GitHub.com/releases/directory"
	const primary = "https://ws.ourworld.top/directory"
	const backup = "https://windshare.cc/directory"
	config := f.config()
	config.Cache = cache
	config.Mirrors = []string{cold, primary, backup}
	var calls []string
	config.Fetch = func(_ context.Context, address string) ([]byte, error) {
		calls = append(calls, address)
		return snapshot, nil
	}
	directory, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	// Let the initial startup refresh complete so the subsequent targeted pass is deterministic.
	if err := directory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	statBefore, err := os.Stat(fileCache.path)
	if err != nil {
		t.Fatal(err)
	}
	oldModTime := statBefore.ModTime().Add(-2 * time.Minute)
	if err := os.Chtimes(fileCache.path, oldModTime, oldModTime); err != nil {
		t.Fatal(err)
	}
	calls = nil
	cache.updates.Store(0)
	if err := directory.RefreshAliases(t.Context(), []string{"missing"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{primary, backup, cold}) {
		t.Fatalf("mirror order = %v, want live mirrors before mixed-case GitHub asset", calls)
	}
	if got := cache.updates.Load(); got != 1 {
		t.Fatalf("cache updates across equal-cursor mirrors = %d, want 1", got)
	}
	statAfter, err := os.Stat(fileCache.path)
	if err != nil {
		t.Fatal(err)
	}
	if !statAfter.ModTime().Equal(oldModTime) {
		t.Fatalf("file cache rewrote identical record: before=%v after=%v", oldModTime, statAfter.ModTime())
	}
}
