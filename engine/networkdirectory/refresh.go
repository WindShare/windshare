package networkdirectory

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
)

type refreshCycle struct {
	done    chan struct{}
	targets map[string]struct{}
	err     error
}

func releaseAsset(mirror string) bool {
	address, err := url.Parse(mirror)
	return err == nil && address.Hostname() == "github.com"
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
	if cycle == nil && now.Before(directory.nextRefresh) && (directory.failures > 0 || scheduled) {
		if directory.failures == 0 {
			directory.mu.Unlock()
			return nil
		}
		err := directory.unavailableLocked()
		directory.mu.Unlock()
		directory.emitRefreshDeferred(now, err)
		return err
	}
	if cycle == nil {
		cycle = &refreshCycle{done: make(chan struct{}), targets: make(map[string]struct{})}
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
	if err == nil {
		directory.failures = 0
		directory.nextRefresh = now.Add(RefreshInterval)
		directory.earliestTrigger = now.Add(MinimumBackoff)
	} else {
		directory.failures = min(directory.failures+1, 10)
		backoff := min(MinimumBackoff*time.Duration(1<<(directory.failures-1)), MaximumBackoff)
		directory.nextRefresh = now.Add(backoff)
	}
	// Sink.Emit is strictly nonblocking; emitting before clearing refreshing and
	// closing done keeps both Refresh callers and Close ordered after publication.
	directory.emitRefreshCompleted(now, directory.highest, directory.failures, directory.nextRefresh, err)
	directory.refreshing = nil
	directory.notifySchedule()
	if err != nil {
		cycle.err = directory.unavailableLocked()
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
	var accepted bool
	var failures []error
	for attempt, index := range order {
		if attempt >= MirrorsPerRefresh {
			break
		}
		mirror := directory.config.Mirrors[index]
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
			err = directory.install(ctx, snapshot)
		}
		if err == nil {
			stage = refreshStageAccepted
		}
		finished := directory.config.Now()
		directory.emitMirrorAttempt(finished, mirror, stage, snapshot, finished.Sub(started), err)
		if err == nil {
			accepted = true
			directory.mu.Lock()
			complete := true
			for alias := range cycle.targets {
				if _, ok := snapshot.Resolve(alias); !ok {
					complete = false
					break
				}
			}
			if complete {
				directory.finishLocked(cycle, nil)
			}
			directory.mu.Unlock()
			if complete {
				return nil
			}
			continue
		}
		failures = append(failures, err)
		if ctx.Err() != nil {
			break
		}
	}
	if accepted {
		directory.mu.Lock()
		directory.finishLocked(cycle, nil)
		directory.mu.Unlock()
		return nil
	}
	return errors.Join(append(failures, nodedirectory.ErrExpired)...)
}
