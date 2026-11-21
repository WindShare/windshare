package networkdirectory

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
)

type refreshCycle struct {
	done              chan struct{}
	targets           map[string]struct{}
	networkGeneration uint64
	err               error
}

func releaseAsset(mirror string) bool {
	address, err := url.Parse(mirror)
	return err == nil && strings.EqualFold(address.Hostname(), "github.com")
}

// RefreshAliases joins the current pass, including startup maintenance, and
// asks it to continue past usable snapshots that still lack these aliases.
func (directory *Directory) RefreshAliases(ctx context.Context, aliases []string) error {
	return directory.requestRefresh(ctx, false, aliases)
}

func (directory *Directory) requestRefresh(ctx context.Context, scheduled bool, aliases []string) error {
	directory.mu.Lock()
	if err := directory.ctx.Err(); err != nil {
		directory.mu.Unlock()
		return err
	}
	cycle := directory.refreshing
	now := directory.config.Now()
	if cycle == nil {
		if scheduled {
			if now.Before(directory.nextRefresh) {
				directory.mu.Unlock()
				return nil
			}
		} else if directory.failures > 0 && now.Before(directory.explicitRetryAt) {
			err := directory.unavailableLocked(now)
			directory.mu.Unlock()
			directory.emitRefreshDeferred(now, err)
			return err
		}
		cycle = &refreshCycle{done: make(chan struct{}), targets: make(map[string]struct{}), networkGeneration: directory.networkGeneration}
		directory.refreshing = cycle
		go directory.refresh(cycle)
	}
	for _, alias := range aliases {
		cycle.targets[alias] = struct{}{}
	}
	directory.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-directory.ctx.Done():
		return directory.ctx.Err()
	case <-cycle.done:
		return cycle.err
	}
}

// finishLocked makes completion and the decision about outstanding aliases atomic.
func (directory *Directory) finishLocked(cycle *refreshCycle, err error) {
	directory.lastError = err
	now := directory.config.Now()
	switch {
	case err == nil:
		directory.failures = 0
		directory.nextRefresh = now.Add(RefreshInterval)
		directory.earliestTrigger = now.Add(MinimumBackoff)
		directory.explicitRetryAt = time.Time{}
	case cycle.networkGeneration != directory.networkGeneration:
		// A network transition occurred while this pass was in flight, so its
		// failure reflects the previous network rather than the current one.
		directory.failures = 0
		directory.nextRefresh = now
		directory.explicitRetryAt = time.Time{}
	default:
		directory.failures = min(directory.failures+1, 10)
		backoff := min(MinimumBackoff*time.Duration(1<<(directory.failures-1)), MaximumBackoff)
		directory.nextRefresh = now.Add(backoff)
		directory.explicitRetryAt = now.Add(min(backoff, ExplicitRetryCooldown))
	}
	// Sink.Emit is strictly nonblocking; emitting before clearing refreshing and
	// closing done keeps both Refresh callers and Close ordered after publication.
	directory.emitRefreshCompleted(now, directory.highest, directory.failures, directory.nextRefresh, directory.explicitRetryAt, err)
	directory.refreshing = nil
	directory.notifySchedule()
	if err != nil {
		cycle.err = directory.unavailableLocked(now)
	}
	close(cycle.done)
}

func (directory *Directory) refresh(cycle *refreshCycle) {
	ctx, cancel := context.WithTimeout(directory.ctx, RefreshTimeout)
	defer cancel()
	err := directory.tryMirrors(ctx, cycle)
	if err != nil {
		directory.mu.Lock()
		directory.finishLocked(cycle, err)
		directory.mu.Unlock()
	}
}

func (directory *Directory) finishIfResolved(cycle *refreshCycle, snapshot nodedirectory.Snapshot) bool {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	for alias := range cycle.targets {
		if _, ok := snapshot.Resolve(alias); !ok {
			return false
		}
	}
	directory.finishLocked(cycle, nil)
	return true
}

func (directory *Directory) tryMirrors(ctx context.Context, cycle *refreshCycle) error {
	if len(directory.config.Roots) == 0 {
		return blocked(Unprovisioned, nil)
	}
	order := directory.config.Order(len(directory.config.Mirrors))
	// Release configuration orders live mirrors before the cold release asset.
	// Randomize peers within each tier so neither primary absorbs every refresh.
	for _, index := range order {
		if index < 0 || index >= len(directory.config.Mirrors) {
			return nodedirectory.ErrFormat
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		return !releaseAsset(directory.config.Mirrors[order[i]]) && releaseAsset(directory.config.Mirrors[order[j]])
	})
	var committed *nodedirectory.Cursor
	var failures []error
	for attempt, index := range order {
		if attempt >= MirrorsPerRefresh {
			break
		}
		mirror := directory.config.Mirrors[index]
		snapshot, err := directory.fetchMirror(ctx, mirror, committed)
		if err == nil {
			cursor := snapshot.Cursor()
			committed = &cursor
			if directory.finishIfResolved(cycle, snapshot) {
				return nil
			}
			continue
		}
		failures = append(failures, err)
		if ctx.Err() != nil {
			break
		}
	}
	if committed != nil {
		directory.mu.Lock()
		directory.finishLocked(cycle, nil)
		directory.mu.Unlock()
		return nil
	}
	return errors.Join(append(failures, nodedirectory.ErrExpired)...)
}

func (directory *Directory) fetchMirror(ctx context.Context, mirror string, committed *nodedirectory.Cursor) (nodedirectory.Snapshot, error) {
	request, cancel := context.WithTimeout(ctx, MirrorTimeout)
	started := directory.config.Now()
	stage := refreshStageFetch
	data, err := directory.config.Fetch(request, mirror)
	cancel()
	var snapshot nodedirectory.Snapshot
	if err == nil {
		stage = refreshStageVerify
		snapshot, err = nodedirectory.Verify(data, directory.config.Network, directory.config.Roots)
	}
	if err == nil {
		stage = refreshStageInstall
		// Skip redundant durable lock/write passes when another mirror in
		// this same cycle already committed the exact same signed cursor.
		if committed != nil && snapshot.Cursor() == *committed {
			if !snapshot.Usable(directory.config.Now(), nodedirectory.DefaultGrace) {
				err = nodedirectory.ErrExpired
			}
		} else {
			err = directory.install(ctx, snapshot)
		}
	}
	if err == nil {
		stage = refreshStageAccepted
	}
	finished := directory.config.Now()
	directory.emitMirrorAttempt(finished, mirror, stage, snapshot, finished.Sub(started), err)
	return snapshot, err
}
