package networkdirectory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
)

func TestUnavailableRefreshPublishesBackoffAndRetainsUsableSnapshot(t *testing.T) {
	for _, embedded := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "usable"}[embedded], func(t *testing.T) {
			f := signing()
			config := f.config()
			var clock atomic.Int64
			clock.Store(f.now.UnixNano())
			config.Now = func() time.Time { return time.Unix(0, clock.Load()) }
			if embedded {
				config.Embedded = f.envelope(t, 1, 1, 0)
			}
			config.Mirrors = []string{"https://mirror.example"}
			var calls atomic.Int32
			var online atomic.Bool
			data := f.envelope(t, 1, 2, 0)
			cause := errors.New("offline")
			config.Fetch = func(context.Context, string) ([]byte, error) {
				calls.Add(1)
				if !online.Load() {
					return nil, cause
				}
				return data, nil
			}
			directory, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			defer directory.Close()
			err = directory.Refresh(context.Background())
			var failure *UnavailableError
			if !errors.As(err, &failure) || !errors.Is(err, cause) || failure.Reason != SnapshotUnavailable || !failure.RetryAt.Equal(f.now.Add(MinimumBackoff)) {
				t.Fatal("missing typed failure/backoff", err)
			}
			if got := directory.Unavailability(); embedded && got != nil || !embedded && got == nil {
				t.Fatal("maintenance failure overrode snapshot availability", got)
			}
			count := calls.Load()
			for range 5 {
				if err := directory.Refresh(context.Background()); !errors.As(err, &failure) || !failure.RetryAt.Equal(f.now.Add(MinimumBackoff)) {
					t.Fatal(err)
				}
			}
			if calls.Load() != count {
				t.Fatal("retry bypassed cooldown")
			}
			online.Store(true)
			clock.Store(f.now.Add(MinimumBackoff).UnixNano())
			if err := directory.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			if directory.Unavailability() != nil || !directory.RetryAt().IsZero() {
				t.Fatal("successful refresh retained failure backoff")
			}
		})
	}
}

type failingTrustCache struct {
	MemoryCache
	load, save error
}

func (cache *failingTrustCache) Load(ctx context.Context) (Record, error) {
	if cache.load != nil {
		return Record{}, cache.load
	}
	return cache.MemoryCache.Load(ctx)
}
func (cache *failingTrustCache) Update(ctx context.Context, change func(Record) (Record, error)) (Record, error) {
	if cache.save != nil {
		return Record{}, cache.save
	}
	return cache.MemoryCache.Update(ctx, change)
}

func TestTrustFailuresRemainTypedAndDoNotResetLocalState(t *testing.T) {
	f := signing()
	for _, test := range []struct {
		name   string
		cache  Cache
		reason BlockingReason
	}{
		{"read", &failingTrustCache{load: os.ErrPermission}, TrustStateUnreadable},
		{"write", &failingTrustCache{save: os.ErrPermission}, TrustStateUnwritable},
		{"cursor", &MemoryCache{record: Record{Cursor: nodedirectory.Cursor{Generation: 2}}}, TrustStateInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := f.config()
			config.Embedded, config.Cache = f.envelope(t, 1, 1, 0), test.cache
			directory, err := New(config)
			var failure *UnavailableError
			if directory != nil || !errors.As(err, &failure) || failure.Reason != test.reason {
				t.Fatal(directory, err)
			}
		})
	}
	cache := NewFileCache(filepath.Join(t.TempDir(), "config.json"))
	if err := os.MkdirAll(filepath.Dir(cache.path), 0700); err != nil {
		t.Fatal(err)
	}
	const broken = "{invalid"
	if err := os.WriteFile(cache.path, []byte(broken), 0600); err != nil {
		t.Fatal(err)
	}
	config := f.config()
	config.Cache = cache
	_, err := New(config)
	var failure *UnavailableError
	if !errors.As(err, &failure) || failure.Reason != TrustStateInvalid {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cache.path)
	if err != nil || string(data) != broken {
		t.Fatal("trust cache was repaired implicitly", err)
	}
}

func TestUnprovisionedDirectoryHasNoNetworkRetryAction(t *testing.T) {
	directory, err := New(Config{Network: "unprovisioned"})
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	failure := directory.Unavailability()
	if failure == nil || failure.Reason != Unprovisioned || !failure.RetryAt.IsZero() {
		t.Fatal(failure)
	}
}
