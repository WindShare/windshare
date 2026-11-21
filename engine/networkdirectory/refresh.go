package networkdirectory

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
)

type refreshCycle struct {
	done              chan struct{}
	targets           map[string]struct{}
	networkGeneration uint64
	initialCursor     nodedirectory.Cursor
	triggered         bool
	err               error
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
		triggered := scheduled && directory.triggerPending
		directory.triggerPending = false
		cycle = &refreshCycle{
			done:              make(chan struct{}),
			targets:           make(map[string]struct{}),
			networkGeneration: directory.networkGeneration,
			initialCursor:     directory.highest,
			triggered:         triggered,
		}
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
		directory.triggerPending = false
		if cycle.triggered && cycle.networkGeneration == directory.networkGeneration && directory.highest == cycle.initialCursor {
			directory.unchangedTriggers = min(directory.unchangedTriggers+1, maxBackoffShift)
		} else {
			directory.unchangedTriggers = 0
		}
		triggerCooldown := min(MinimumBackoff<<directory.unchangedTriggers, MaximumTriggerBackoff)
		directory.nextRefresh = now.Add(RefreshInterval)
		directory.earliestTrigger = now.Add(triggerCooldown)
		directory.explicitRetryAt = time.Time{}
	case cycle.networkGeneration != directory.networkGeneration:
		// A network transition occurred while this pass was in flight, so its
		// failure reflects the previous network rather than the current one.
		directory.failures = 0
		directory.unchangedTriggers = 0
		directory.triggerPending = false
		directory.nextRefresh = now
		directory.explicitRetryAt = time.Time{}
	default:
		directory.failures = min(directory.failures+1, maxBackoffShift)
		directory.triggerPending = false
		backoff := min(MinimumBackoff*time.Duration(1<<(directory.failures-1)), MaximumBackoff)
		directory.nextRefresh = now.Add(backoff)
		directory.explicitRetryAt = now.Add(min(backoff, ExplicitRetryCooldown))
	}
	// Sink.Emit is strictly nonblocking; emitting before clearing refreshing and
	// closing done keeps both Refresh callers and Close ordered after publication.
	directory.emitRefreshCompleted(now, directory.highest, directory.failures, directory.unchangedTriggers, directory.nextRefresh, directory.earliestTrigger, directory.explicitRetryAt, err)
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

func (directory *Directory) orderedTier(mirrors []string) ([]string, error) {
	if len(mirrors) == 0 {
		return nil, nil
	}
	order := directory.config.Order(len(mirrors))
	ordered := make([]string, 0, len(order))
	for _, index := range order {
		if index < 0 || index >= len(mirrors) {
			return nil, nodedirectory.ErrFormat
		}
		ordered = append(ordered, mirrors[index])
	}
	return ordered, nil
}

func (directory *Directory) tryMirrors(ctx context.Context, cycle *refreshCycle) error {
	if len(directory.config.Roots) == 0 {
		return blocked(Unprovisioned, nil)
	}
	live, err := directory.orderedTier(directory.config.Mirrors)
	if err != nil {
		return err
	}
	var committed *nodedirectory.Cursor
	var failures []error
	seen := make(map[string]struct{}, MirrorsPerRefresh)
	attempts := 0
	tryTier := func(mirrors []string, tier string) bool {
		for _, mirror := range mirrors {
			if attempts >= MirrorsPerRefresh || ctx.Err() != nil {
				break
			}
			if _, duplicate := seen[mirror]; duplicate {
				continue
			}
			seen[mirror] = struct{}{}
			attempts++
			snapshot, err := directory.fetchMirror(ctx, mirror, tier, committed)
			if err == nil {
				cursor := snapshot.Cursor()
				committed = &cursor
				if directory.finishIfResolved(cycle, snapshot) {
					return true
				}
				continue
			}
			failures = append(failures, err)
		}
		return false
	}
	if tryTier(live, mirrorTierLive) {
		return nil
	}
	// Cold fallback mirrors carry static release snapshots and cannot advertise
	// newer aliases than a reachable live mirror; use them only when all live
	// mirrors fail.
	if committed == nil && ctx.Err() == nil && len(directory.config.FallbackMirrors) > 0 {
		fallback, err := directory.orderedTier(directory.config.FallbackMirrors)
		if err != nil {
			return err
		}
		if tryTier(fallback, mirrorTierFallback) {
			return nil
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

func (directory *Directory) fetchMirror(ctx context.Context, mirror, tier string, committed *nodedirectory.Cursor) (nodedirectory.Snapshot, error) {
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
	directory.emitMirrorAttempt(finished, mirror, tier, stage, snapshot, finished.Sub(started), err)
	return snapshot, err
}
