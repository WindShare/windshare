package networkdirectory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
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
		const cold = "https://release-asset.example/directory"
		const primary = "https://ws.ourworld.top/directory"
		const backup = "https://windshare.cc/directory"
		stream, err := diagnostics.NewStream(32, nil)
		if err != nil {
			t.Fatal(err)
		}
		config := f.config()
		config.Mirrors = []string{primary, backup}
		config.FallbackMirrors = []string{cold}
		config.Diagnostics = stream.Sink()
		began, unblock := make(chan struct{}), make(chan struct{})
		var calls []string
		var liveDown atomic.Bool
		config.Fetch = func(_ context.Context, address string) ([]byte, error) {
			calls = append(calls, address)
			if len(calls) == 1 {
				close(began)
				<-unblock
				return stale, nil
			}
			if liveDown.Load() && address != cold {
				return nil, errors.New("live mirror outage")
			}
			return fresh, nil
		}
		directory, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
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
		// Missing aliases must stop after live mirrors succeed without probing cold fallbacks.
		if err := directory.RefreshAliases(t.Context(), []string{"nonexistent"}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(calls[2:], []string{primary, backup}) {
			t.Fatalf("targeted pass probed cold fallback after live success: %v", calls[2:])
		}
		if err := directory.Refresh(t.Context()); err != nil || calls[len(calls)-1] != primary {
			t.Fatalf("unexpected backoff: %v", err)
		}
		// When every live mirror fails, cold fallback recovers availability.
		beforeOutage := len(calls)
		liveDown.Store(true)
		if err := directory.Refresh(t.Context()); err != nil {
			t.Fatalf("cold fallback did not recover live outage: %v", err)
		}
		if !slices.Equal(calls[beforeOutage:], []string{primary, backup, cold}) {
			t.Fatalf("outage pass used %v, want live mirrors before cold fallback", calls[beforeOutage:])
		}
		directory.Close()
		stream.Close()
		var sawFallbackAccepted bool
		for record := range stream.Records() {
			if record.Name() == "network_directory_refresh" &&
				record.Payload().Get("mirror").Text() == cold &&
				record.Payload().Get("tier").Text() == mirrorTierFallback &&
				record.Payload().Get("stage").Text() == refreshStageAccepted {
				sawFallbackAccepted = true
			}
		}
		if !sawFallbackAccepted {
			t.Fatal("missing fallback tier diagnostic on recovery")
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

func TestEqualCursorLiveMirrorsAvoidRedundantWritesAndSkipFallback(t *testing.T) {
	f := signing()
	snapshot := f.envelope(t, 1, 1, 0)
	fileCache := NewFileCache(filepath.Join(t.TempDir(), "config.json"), f.config().Network)
	cache := &countingCache{inner: fileCache}
	const cold = "https://release-asset.example/directory"
	const primary = "https://ws.ourworld.top/directory"
	const backup = "https://windshare.cc/directory"
	config := f.config()
	config.Cache = cache
	config.Mirrors = []string{primary, backup}
	config.FallbackMirrors = []string{cold}
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
	if !slices.Equal(calls, []string{primary, backup}) {
		t.Fatalf("mirror order = %v, want live mirrors only when a live mirror succeeded", calls)
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

func TestMirrorTierConfigurationValidation(t *testing.T) {
	f := signing()
	invalid := f.config()
	invalid.FallbackMirrors = []string{"http://insecure.example/directory"}
	if _, err := New(invalid); !errors.Is(err, nodedirectory.ErrFormat) {
		t.Fatalf("non-https fallback mirror err = %v, want ErrFormat", err)
	}
	tooMany := f.config()
	tooMany.Mirrors = []string{
		"https://m1.example", "https://m2.example", "https://m3.example", "https://m4.example",
		"https://m5.example", "https://m6.example", "https://m7.example",
	}
	tooMany.FallbackMirrors = []string{"https://f1.example", "https://f2.example"}
	if _, err := New(tooMany); !errors.Is(err, nodedirectory.ErrFormat) {
		t.Fatalf("combined mirrors over limit err = %v, want ErrFormat", err)
	}
}

func TestExplicitAliasRefreshCapsCooldownWithoutShorteningBackgroundBackoff(t *testing.T) {
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

		start := time.Now()
		config := f.config()
		config.Embedded = stale
		config.Mirrors = []string{"https://mirror.example/directory"}
		config.Now = func() time.Time { return f.now.Add(time.Since(start)) }
		var calls atomic.Int32
		var online atomic.Bool
		config.Fetch = func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			if !online.Load() {
				return nil, errors.New("offline")
			}
			return fresh, nil
		}

		directory, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		defer directory.Close()

		// Drive scheduled backoff to MaximumBackoff (30s + 1m + 2m + 4m + 8m + 16m = 31m30s).
		synctest.Wait()
		for _, step := range []time.Duration{
			MinimumBackoff,
			2 * MinimumBackoff,
			4 * MinimumBackoff,
			8 * MinimumBackoff,
			16 * MinimumBackoff,
			32 * MinimumBackoff,
		} {
			time.Sleep(step)
			synctest.Wait()
		}
		if got := calls.Load(); got != 7 {
			t.Fatalf("background failure attempts = %d, want 7", got)
		}
		now := config.Now()
		if got := directory.RetryAt(); !got.Equal(now.Add(ExplicitRetryCooldown)) {
			t.Fatalf("RetryAt = %v, want explicit cap %v", got, now.Add(ExplicitRetryCooldown))
		}
		var failure *UnavailableError
		if err := directory.RefreshAliases(t.Context(), []string{"new"}); !errors.As(err, &failure) || !failure.RetryAt.Equal(now.Add(ExplicitRetryCooldown)) {
			t.Fatalf("immediate explicit alias refresh = %v, want explicit cooldown", err)
		}
		if calls.Load() != 7 {
			t.Fatalf("explicit cooldown did not throttle immediate retry: calls=%d", calls.Load())
		}

		// After ExplicitRetryCooldown (30s), background remains in 30m backoff while explicit refresh proceeds.
		time.Sleep(ExplicitRetryCooldown)
		synctest.Wait()
		if calls.Load() != 7 {
			t.Fatalf("background refresh ignored 30m backoff: calls=%d", calls.Load())
		}
		if !directory.RetryAt().IsZero() {
			t.Fatalf("expired explicit cooldown remained active: %v", directory.RetryAt())
		}
		if err := directory.RefreshAliases(t.Context(), []string{"new"}); !errors.As(err, &failure) || !failure.RetryAt.Equal(config.Now().Add(ExplicitRetryCooldown)) {
			t.Fatalf("explicit retry after cooldown = %v, want new %v cooldown", err, ExplicitRetryCooldown)
		}
		if calls.Load() != 8 {
			t.Fatalf("explicit alias refresh stayed blocked during 30m background backoff: calls=%d", calls.Load())
		}

		// Background must still wait the full MaximumBackoff after the failed explicit refresh.
		time.Sleep(ExplicitRetryCooldown)
		synctest.Wait()
		if calls.Load() != 8 {
			t.Fatalf("background refresh ran before MaximumBackoff: calls=%d", calls.Load())
		}
		online.Store(true)
		if err := directory.RefreshAliases(t.Context(), []string{"new"}); err != nil {
			t.Fatalf("explicit alias refresh after recovery failed: %v", err)
		}
		snapshot, usable := directory.Snapshot()
		if _, ok := snapshot.Resolve("new"); !usable || !ok || !directory.RetryAt().IsZero() {
			t.Fatalf("recovered snapshot missing alias or retained cooldown: usable=%t ok=%t retry=%v", usable, ok, directory.RetryAt())
		}
	})
}

func TestNetworkChangedResetsBackoffAndSupersedesInFlightFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stream, err := diagnostics.NewStream(32, nil)
		if err != nil {
			t.Fatal(err)
		}
		f := signing()
		data := f.envelope(t, 1, 1, 0)
		start := time.Now()
		config := f.config()
		config.Mirrors = []string{"https://mirror.example/directory"}
		config.Now = func() time.Time { return f.now.Add(time.Since(start)) }
		config.Diagnostics = stream.Sink()

		var calls atomic.Int32
		var online atomic.Bool
		var holdNext atomic.Bool
		entered, release := make(chan struct{}), make(chan struct{})
		config.Fetch = func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			if holdNext.Swap(false) {
				close(entered)
				<-release
				return nil, errors.New("stale network failure")
			}
			if !online.Load() {
				return nil, errors.New("offline")
			}
			return data, nil
		}

		directory, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if calls.Load() != 1 || directory.RetryAt().IsZero() {
			t.Fatalf("expected initial failure backoff: calls=%d retry=%v", calls.Load(), directory.RetryAt())
		}

		// Network change while in backoff clears cooldown and recovers immediately.
		online.Store(true)
		directory.NetworkChanged()
		synctest.Wait()
		if calls.Load() != 2 || !directory.RetryAt().IsZero() {
			t.Fatalf("network change did not trigger immediate recovery: calls=%d retry=%v", calls.Load(), directory.RetryAt())
		}
		if _, usable := directory.Snapshot(); !usable {
			t.Fatal("snapshot not installed after network-change recovery")
		}

		// Healthy directory must not issue extra fetches on subsequent network changes.
		directory.NetworkChanged()
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("healthy directory fetched on network change: calls=%d", calls.Load())
		}

		// An in-flight failure from the previous network generation must not impose cooldown.
		holdNext.Store(true)
		done := make(chan error, 1)
		go func() { done <- directory.Refresh(t.Context()) }()
		<-entered
		directory.NetworkChanged()
		close(release)
		var failure *UnavailableError
		if err := <-done; !errors.As(err, &failure) || !failure.RetryAt.IsZero() {
			t.Fatalf("superseded in-flight failure carried backoff: %v", err)
		}
		synctest.Wait()
		if calls.Load() != 4 || !directory.RetryAt().IsZero() {
			t.Fatalf("superseded in-flight failure did not immediately retry on new network: calls=%d retry=%v", calls.Load(), directory.RetryAt())
		}

		directory.Close()
		stream.Close()
		var sawReset bool
		for record := range stream.Records() {
			if record.Name() == "network_directory_backoff_reset" &&
				record.Payload().Get("previous_failures").Uint() == 1 &&
				record.Payload().Get("reason").Text() == "network_changed" {
				sawReset = true
			}
		}
		if !sawReset {
			t.Fatal("missing network_directory_backoff_reset diagnostic")
		}
	})
}

func TestTriggeredRefreshBacksOffWhenCursorIsUnchangedAndResetsOnAdvance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stream, err := diagnostics.NewStream(64, nil)
		if err != nil {
			t.Fatal(err)
		}
		f := signing()
		stale := f.envelope(t, 1, 1, 0)
		fresh := f.envelope(t, 1, 2, 0)
		start := time.Now()
		config := f.config()
		config.Embedded = stale
		config.Mirrors = []string{"https://mirror.example/directory"}
		config.Now = func() time.Time { return f.now.Add(time.Since(start)) }
		config.Diagnostics = stream.Sink()

		var calls atomic.Int32
		var publishFresh atomic.Bool
		config.Fetch = func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			if publishFresh.Load() {
				return fresh, nil
			}
			return stale, nil
		}

		directory, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("startup calls = %d, want 1", calls.Load())
		}

		// Repeated unchanged triggered fetches back off 30s -> 60s -> 120s -> 240s -> 5m cap.
		steps := []time.Duration{
			MinimumBackoff,
			2 * MinimumBackoff,
			4 * MinimumBackoff,
			8 * MinimumBackoff,
			MaximumTriggerBackoff,
		}
		for i, wait := range steps {
			wantBefore := int32(i + 1)
			wantAfter := int32(i + 2)
			directory.TriggerRefresh()
			synctest.Wait()
			time.Sleep(wait - time.Nanosecond)
			synctest.Wait()
			if calls.Load() != wantBefore {
				t.Fatalf("step %d fetched early before %v: calls=%d want=%d", i, wait, calls.Load(), wantBefore)
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			if calls.Load() != wantAfter {
				t.Fatalf("step %d did not fetch at %v: calls=%d want=%d", i, wait, calls.Load(), wantAfter)
			}
		}

		// Trigger backoff must not throttle an explicit Refresh, and explicit success resets trigger backoff.
		if err := directory.Refresh(t.Context()); err != nil {
			t.Fatalf("explicit Refresh during trigger backoff failed: %v", err)
		}
		if calls.Load() != 7 {
			t.Fatalf("calls after explicit refresh = %d, want 7", calls.Load())
		}

		// Drive one unchanged trigger (30s -> next 60s), then verify NetworkChanged resets it back to 30s without fetching.
		directory.TriggerRefresh()
		time.Sleep(MinimumBackoff)
		synctest.Wait()
		if calls.Load() != 8 {
			t.Fatalf("calls after post-explicit trigger = %d, want 8", calls.Load())
		}
		directory.TriggerRefresh()
		synctest.Wait()
		directory.NetworkChanged()
		synctest.Wait()
		if calls.Load() != 8 {
			t.Fatalf("NetworkChanged unexpectedly fetched while healthy: calls=%d", calls.Load())
		}
		time.Sleep(MinimumBackoff - time.Nanosecond)
		synctest.Wait()
		if calls.Load() != 8 {
			t.Fatalf("trigger ran before MinimumBackoff after NetworkChanged: calls=%d", calls.Load())
		}
		publishFresh.Store(true)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if calls.Load() != 9 {
			t.Fatalf("trigger did not fire at clamped MinimumBackoff after NetworkChanged: calls=%d", calls.Load())
		}

		// Advancing the cursor resets unchanged-trigger backoff back to MinimumBackoff.
		directory.TriggerRefresh()
		time.Sleep(MinimumBackoff)
		synctest.Wait()
		if calls.Load() != 10 {
			t.Fatalf("cursor advance did not reset trigger backoff: calls=%d", calls.Load())
		}

		directory.Close()
		stream.Close()
		var sawUnchangedDiag bool
		for record := range stream.Records() {
			if record.Name() == "network_directory_refresh_completed" &&
				record.Payload().Get("unchanged_triggers").Uint() >= 4 &&
				record.Payload().Get("earliest_trigger").Text() != "" {
				sawUnchangedDiag = true
			}
		}
		if !sawUnchangedDiag {
			t.Fatal("missing unchanged_triggers diagnostic on backed-off refresh")
		}
	})
}
