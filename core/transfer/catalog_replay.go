package transfer

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/catalog"
)

var errCatalogReplayClosed = errors.New("transfer catalog replay reader is closed")

const (
	// The handoff target matches one catalog session budget. One already-loaded
	// directory may exceed it so either consumer can still make progress; peak
	// retention is therefore target plus at most one largest snapshot.
	catalogReplayMemoryTarget    = catalog.DefaultSessionCatalogMemory
	catalogReplayDirectoryTarget = catalog.MaxPathDepth + 1
)

type catalogReplayRole uint8

const (
	catalogReplayMeasurement catalogReplayRole = 1 << iota
	catalogReplayExecution
)

// catalogReplay gives the measurement and execution walks one frozen result per
// directory. Retention is a bounded handoff queue: measurement may lead content
// execution, but it cannot materialize a whole-selection snapshot map.
type catalogReplay struct {
	source CatalogReader
	load   chan struct{}

	mu                   sync.Mutex
	work                 sync.WaitGroup
	entries              map[catalog.DirectoryID]*catalogReplayEntry
	closedRoles          catalogReplayRole
	retainedBytes        uint64
	peakBytes            uint64
	peakSingleEntryBytes uint64
	peakEntries          int
	spaceChanged         chan struct{}
}

type catalogReplayEntry struct {
	done      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	waiters   int
	ready     bool
	snapshot  catalog.DirectorySnapshot
	err       error
	bytes     uint64
	delivered catalogReplayRole
	released  catalogReplayRole
}

type catalogReplayReader struct {
	replay *catalogReplay
	role   catalogReplayRole
	once   sync.Once
}

func newCatalogReplay(source CatalogReader) *catalogReplay {
	return &catalogReplay{
		source: source, load: make(chan struct{}, 1),
		entries: make(map[catalog.DirectoryID]*catalogReplayEntry), spaceChanged: make(chan struct{}),
	}
}

func (replay *catalogReplay) reader(role catalogReplayRole) *catalogReplayReader {
	return &catalogReplayReader{replay: replay, role: role}
}

func (reader *catalogReplayReader) LoadDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, error) {
	return reader.replay.loadDirectory(ctx, reader.role, directory)
}

func (reader *catalogReplayReader) AcquireDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	snapshot, err := reader.LoadDirectory(ctx, directory)
	var once sync.Once
	return snapshot, func() {
		once.Do(func() { reader.ReleaseDirectory(directory) })
	}, err
}

func (reader *catalogReplayReader) ReleaseDirectory(directory catalog.DirectoryID) {
	reader.replay.release(reader.role, directory)
}

func (reader *catalogReplayReader) Close() {
	reader.once.Do(func() { reader.replay.closeRole(reader.role) })
}

func (replay *catalogReplay) loadDirectory(
	ctx context.Context,
	role catalogReplayRole,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, error) {
	replay.mu.Lock()
	if replay.closedRoles&role != 0 {
		replay.mu.Unlock()
		return catalog.DirectorySnapshot{}, errCatalogReplayClosed
	}
	entry := replay.entries[directory]
	if entry == nil {
		loadContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
		entry = &catalogReplayEntry{done: make(chan struct{}), ctx: loadContext, cancel: cancel}
		replay.entries[directory] = entry
		replay.work.Add(1)
		go replay.runLoad(directory, entry)
	}
	entry.waiters++
	replay.mu.Unlock()

	select {
	case <-ctx.Done():
		replay.releaseWaiter(directory, entry)
		return catalog.DirectorySnapshot{}, ctx.Err()
	case <-entry.done:
		replay.markDelivered(role, directory, entry)
		replay.releaseWaiter(directory, entry)
		return entry.snapshot, entry.err
	}
}

func (replay *catalogReplay) runLoad(
	directory catalog.DirectoryID,
	entry *catalogReplayEntry,
) {
	defer replay.work.Done()
	select {
	case replay.load <- struct{}{}:
	case <-entry.ctx.Done():
		replay.publish(directory, entry, catalog.DirectorySnapshot{}, entry.ctx.Err())
		return
	}
	defer func() { <-replay.load }()
	if err := replay.waitForCapacity(entry.ctx); err != nil {
		replay.publish(directory, entry, catalog.DirectorySnapshot{}, err)
		return
	}
	snapshot, release, err := replay.source.AcquireDirectory(entry.ctx, directory)
	if release == nil {
		release = func() {}
		err = errors.Join(NewJobDependencyContractError(ErrCatalogLeaseContract), err)
	}
	// DirectorySnapshot is immutable and owns its Go references. Once captured,
	// replay accounts that value itself; retaining the source cache lease would
	// double-charge the same handoff and can deadlock a bounded source cache.
	release()
	replay.publish(directory, entry, snapshot, err)
}

func (replay *catalogReplay) releaseWaiter(
	directory catalog.DirectoryID,
	entry *catalogReplayEntry,
) {
	replay.mu.Lock()
	if replay.entries[directory] == entry {
		entry.waiters--
		if entry.waiters == 0 && !entry.ready {
			entry.cancel()
		}
	}
	replay.mu.Unlock()
}

func (replay *catalogReplay) waitForCapacity(ctx context.Context) error {
	for {
		replay.mu.Lock()
		withinDirectoryLimit := replay.retainedEntriesLocked() < catalogReplayDirectoryTarget
		withinByteLimit := replay.retainedBytes < catalogReplayMemoryTarget || replay.retainedEntriesLocked() == 0
		if withinDirectoryLimit && withinByteLimit {
			replay.mu.Unlock()
			return nil
		}
		changed := replay.spaceChanged
		replay.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (replay *catalogReplay) publish(
	directory catalog.DirectoryID,
	entry *catalogReplayEntry,
	snapshot catalog.DirectorySnapshot,
	err error,
) {
	replay.mu.Lock()
	entry.snapshot, entry.err = snapshot, err
	entry.bytes = snapshot.EstimatedMemoryBytes()
	entry.ready = true
	replay.retainedBytes += entry.bytes
	if replay.retainedBytes > replay.peakBytes {
		replay.peakBytes = replay.retainedBytes
	}
	if entry.bytes > replay.peakSingleEntryBytes {
		replay.peakSingleEntryBytes = entry.bytes
	}
	if retained := replay.retainedEntriesLocked(); retained > replay.peakEntries {
		replay.peakEntries = retained
	}
	close(entry.done)
	entry.cancel()
	replay.maybeDeleteLocked(directory, entry)
	replay.mu.Unlock()
}

func (replay *catalogReplay) markDelivered(
	role catalogReplayRole,
	directory catalog.DirectoryID,
	entry *catalogReplayEntry,
) {
	replay.mu.Lock()
	if replay.entries[directory] == entry {
		entry.delivered |= role
		replay.maybeDeleteLocked(directory, entry)
	}
	replay.mu.Unlock()
}

func (replay *catalogReplay) release(role catalogReplayRole, directory catalog.DirectoryID) {
	replay.mu.Lock()
	entry := replay.entries[directory]
	if entry != nil {
		entry.released |= role
		replay.maybeDeleteLocked(directory, entry)
	}
	replay.mu.Unlock()
}

func (replay *catalogReplay) closeRole(role catalogReplayRole) {
	replay.mu.Lock()
	replay.closedRoles |= role
	for directory, entry := range replay.entries {
		replay.maybeDeleteLocked(directory, entry)
	}
	replay.mu.Unlock()
}

func (replay *catalogReplay) maybeDeleteLocked(
	directory catalog.DirectoryID,
	entry *catalogReplayEntry,
) {
	if !entry.ready {
		return
	}
	for _, role := range []catalogReplayRole{catalogReplayMeasurement, catalogReplayExecution} {
		if replay.closedRoles&role != 0 {
			continue
		}
		if entry.delivered&role == 0 || entry.released&role == 0 {
			return
		}
	}
	delete(replay.entries, directory)
	replay.retainedBytes -= entry.bytes
	close(replay.spaceChanged)
	replay.spaceChanged = make(chan struct{})
}

func (replay *catalogReplay) Wait() { replay.work.Wait() }

func (replay *catalogReplay) retainedEntriesLocked() int {
	retained := 0
	for _, entry := range replay.entries {
		if entry.ready {
			retained++
		}
	}
	return retained
}

func (replay *catalogReplay) peaks() (int, uint64, uint64) {
	replay.mu.Lock()
	defer replay.mu.Unlock()
	return replay.peakEntries, replay.peakBytes, replay.peakSingleEntryBytes
}
