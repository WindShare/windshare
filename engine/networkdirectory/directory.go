// Package networkdirectory owns signed network snapshots, bounded refresh and
// durable rollback protection. It never imports built-in infrastructure.
package networkdirectory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
)

const (
	RefreshTimeout    = 6 * time.Second
	MirrorTimeout     = 2 * time.Second
	RefreshInterval   = 6 * time.Hour
	MinimumBackoff    = 30 * time.Second
	MaximumBackoff    = 30 * time.Minute
	MaximumMirrors    = 8
	MirrorsPerRefresh = 3
)

type Config struct {
	Network  string
	Roots    []nodedirectory.Root
	Embedded []byte
	Mirrors  []string
	Cache    Cache
	Fetch    func(context.Context, string) ([]byte, error)
	Now      func() time.Time
	Order    func(int) []int
}

type Directory struct {
	config      Config
	mu          sync.Mutex
	current     nodedirectory.Snapshot
	highest     nodedirectory.Cursor
	refreshing  chan struct{}
	nextRefresh time.Time
	failures    uint
	lastError   error
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
}

func New(config Config) (*Directory, error) {
	if config.Network == "" || len(config.Mirrors) > MaximumMirrors {
		return nil, nodedirectory.ErrFormat
	}
	for _, mirror := range config.Mirrors {
		address, err := url.Parse(mirror)
		if err != nil || address.Scheme != "https" || address.Host == "" || address.User != nil || address.Fragment != "" {
			return nil, nodedirectory.ErrFormat
		}
	}
	config.Roots = slices.Clone(config.Roots)
	for index := range config.Roots {
		config.Roots[index].Key = slices.Clone(config.Roots[index].Key)
	}
	config.Embedded = slices.Clone(config.Embedded)
	config.Mirrors = slices.Clone(config.Mirrors)
	if config.Cache == nil {
		config.Cache = &MemoryCache{}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Fetch == nil {
		config.Fetch = fetch
	}
	if config.Order == nil {
		config.Order = rand.Perm
	}
	ctx, cancel := context.WithCancel(context.Background())
	directory := &Directory{config: config, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	record, err := config.Cache.Load(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("read network trust state: %w", err)
	}
	if len(record.Envelope) > 0 {
		snapshot, err := nodedirectory.Verify(record.Envelope, config.Network, config.Roots)
		if err != nil || snapshot.Cursor() != record.Cursor {
			cancel()
			return nil, errors.Join(nodedirectory.ErrRollback, err)
		}
		directory.current, directory.highest = snapshot, record.Cursor
	} else if record.Cursor.Generation != 0 {
		cancel()
		return nil, nodedirectory.ErrRollback
	}
	if len(config.Embedded) > 0 {
		// An expired embedded export is still verifiable history. It cannot
		// displace a newer cached generation, even after that cache expires.
		snapshot, err := nodedirectory.Verify(config.Embedded, config.Network, config.Roots)
		if err != nil {
			cancel()
			return nil, err
		}
		if err := directory.commit(ctx, snapshot); err != nil && !errors.Is(err, nodedirectory.ErrRollback) {
			cancel()
			return nil, err
		}
	}
	go directory.run()
	return directory, nil
}

func (directory *Directory) Snapshot() (nodedirectory.Snapshot, bool) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	return directory.current, directory.current.Usable(directory.config.Now(), nodedirectory.DefaultGrace)
}

func (directory *Directory) install(ctx context.Context, next nodedirectory.Snapshot) error {
	if !next.Usable(directory.config.Now(), nodedirectory.DefaultGrace) {
		return nodedirectory.ErrExpired
	}
	return directory.commit(ctx, next)
}

func (directory *Directory) commit(ctx context.Context, next nodedirectory.Snapshot) error {
	record, err := directory.config.Cache.Update(ctx, func(previous Record) (Record, error) {
		if err := nodedirectory.Compare(previous.Cursor, next.Cursor()); err != nil {
			return Record{}, err
		}
		aliases, err := nodedirectory.RetainAliases(previous.Aliases, next)
		if err != nil {
			return Record{}, err
		}
		return Record{Cursor: next.Cursor(), Envelope: next.Envelope(), Aliases: aliases}, nil
	})
	if err != nil {
		return err
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if err := nodedirectory.Compare(directory.highest, next.Cursor()); err != nil {
		return err
	}
	directory.current, directory.highest = next, record.Cursor
	return nil
}

// Refresh coalesces user retries, unknown-alias resolution and periodic refresh.
// Caller cancellation stops waiting, not an update another task also needs.
func (directory *Directory) Refresh(ctx context.Context) error {
	if err := directory.ctx.Err(); err != nil {
		return err
	}
	directory.mu.Lock()
	done := directory.refreshing
	if done == nil && directory.failures > 0 && directory.config.Now().Before(directory.nextRefresh) {
		err := directory.lastError
		directory.mu.Unlock()
		if err == nil {
			return nil
		}
		return errors.Join(nodedirectory.ErrExpired, err)
	}
	if done == nil {
		done = make(chan struct{})
		directory.refreshing = done
		go directory.refresh(done)
	}
	directory.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-directory.ctx.Done():
		return directory.ctx.Err()
	case <-done:
		directory.mu.Lock()
		err := directory.lastError
		directory.mu.Unlock()
		return err
	}
}

func (directory *Directory) refresh(done chan struct{}) {
	ctx, cancel := context.WithTimeout(directory.ctx, RefreshTimeout)
	defer cancel()
	err := directory.tryMirrors(ctx)
	directory.mu.Lock()
	defer directory.mu.Unlock()
	directory.lastError = err
	if err == nil {
		directory.failures = 0
		directory.nextRefresh = directory.config.Now().Add(RefreshInterval)
	} else {
		directory.failures = min(directory.failures+1, 10)
		backoff := min(MinimumBackoff*time.Duration(1<<(directory.failures-1)), MaximumBackoff)
		directory.nextRefresh = directory.config.Now().Add(backoff)
	}
	directory.refreshing = nil
	close(done)
}

func (directory *Directory) tryMirrors(ctx context.Context) error {
	if len(directory.config.Roots) == 0 {
		return nodedirectory.ErrExpired
	}
	order := directory.config.Order(len(directory.config.Mirrors))
	var failures []error
	for attempt, index := range order {
		if attempt >= MirrorsPerRefresh {
			break
		}
		if index < 0 || index >= len(directory.config.Mirrors) {
			return nodedirectory.ErrFormat
		}
		mirror := directory.config.Mirrors[index]
		request, cancel := context.WithTimeout(ctx, MirrorTimeout)
		started := directory.config.Now()
		data, err := directory.config.Fetch(request, mirror)
		cancel()
		var snapshot nodedirectory.Snapshot
		if err == nil {
			snapshot, err = nodedirectory.Verify(data, directory.config.Network, directory.config.Roots)
		}
		if err == nil {
			err = directory.install(ctx, snapshot)
		}
		cursor := snapshot.Cursor()
		slog.Debug("network_directory_refresh", "mirror", mirror, "generation", cursor.Generation, "serial", cursor.Serial,
			"duration", directory.config.Now().Sub(started), "trusted_until", snapshot.TrustedUntil(nodedirectory.DefaultGrace), "error", err)
		if err == nil {
			return nil
		}
		failures = append(failures, err)
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(append(failures, nodedirectory.ErrExpired)...)
}

func (directory *Directory) run() {
	defer close(directory.done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-directory.ctx.Done():
			return
		case <-timer.C:
		}
		_ = directory.Refresh(directory.ctx)
		directory.mu.Lock()
		delay := max(MinimumBackoff, directory.nextRefresh.Sub(directory.config.Now()))
		directory.mu.Unlock()
		timer.Reset(delay)
	}
}

func (directory *Directory) Close() {
	directory.cancel()
	<-directory.done
	directory.mu.Lock()
	refreshing := directory.refreshing
	directory.mu.Unlock()
	if refreshing != nil {
		<-refreshing
	}
}
