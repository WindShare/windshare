package transfer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/catalogflow"
)

func replayDirectoryID(index int) catalog.DirectoryID {
	var directory catalog.DirectoryID
	binary.BigEndian.PutUint32(directory[len(directory)-4:], uint32(index+1))
	return directory
}

func replayFileID(index int) catalog.FileID {
	var file catalog.FileID
	binary.BigEndian.PutUint32(file[len(file)-4:], uint32(index+1))
	return file
}

type replayCatalogSource struct {
	snapshots map[catalog.DirectoryID]catalog.DirectorySnapshot
	calls     atomic.Int32
	releases  atomic.Int32
	started   chan catalog.DirectoryID
	gate      <-chan struct{}
}

func (source *replayCatalogSource) AcquireDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	snapshot, err := source.LoadDirectory(ctx, directory)
	return snapshot, func() { source.releases.Add(1) }, err
}

func (source *replayCatalogSource) LoadDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, error) {
	source.calls.Add(1)
	if source.started != nil {
		source.started <- directory
	}
	if source.gate != nil {
		select {
		case <-ctx.Done():
			return catalog.DirectorySnapshot{}, ctx.Err()
		case <-source.gate:
		}
	}
	return source.snapshots[directory], nil
}

func TestCatalogReplayFetchesOnceAcrossSequentialJobConsumers(t *testing.T) {
	share := transferID[catalog.ShareInstance](110)
	directory := transferID[catalog.DirectoryID](111)
	source := &replayCatalogSource{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
		directory: jobSnapshot(t, share, directory, 1),
	}}
	replay := newCatalogReplay(source)
	measurement := replay.reader(catalogReplayMeasurement)
	execution := replay.reader(catalogReplayExecution)

	first, err := measurement.LoadDirectory(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	measurement.ReleaseDirectory(directory)
	if source.releases.Load() != 1 {
		t.Fatalf("source lease remained pinned during replay handoff: releases=%d", source.releases.Load())
	}
	second, err := execution.LoadDirectory(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	execution.ReleaseDirectory(directory)
	measurement.Close()
	execution.Close()
	replay.Wait()
	if !first.Equal(second) || source.calls.Load() != 1 {
		t.Fatalf("equal=%v source calls=%d", first.Equal(second), source.calls.Load())
	}
	if source.releases.Load() != 1 {
		t.Fatalf("source releases=%d", source.releases.Load())
	}
}

func TestCatalogReplayWaiterCancellationDoesNotCancelAnotherConsumer(t *testing.T) {
	share := transferID[catalog.ShareInstance](112)
	directory := transferID[catalog.DirectoryID](113)
	started := make(chan catalog.DirectoryID, 1)
	gate := make(chan struct{})
	source := &replayCatalogSource{
		snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
			directory: jobSnapshot(t, share, directory, 1),
		},
		started: started, gate: gate,
	}
	replay := newCatalogReplay(source)
	execution := replay.reader(catalogReplayExecution)
	measurement := replay.reader(catalogReplayMeasurement)
	executionResult := make(chan error, 1)
	go func() {
		_, err := execution.LoadDirectory(context.Background(), directory)
		executionResult <- err
	}()
	<-started
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := measurement.LoadDirectory(cancelled, directory); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter error=%v", err)
	}
	measurement.Close()
	close(gate)
	if err := <-executionResult; err != nil {
		t.Fatalf("surviving waiter error=%v", err)
	}
	execution.ReleaseDirectory(directory)
	execution.Close()
	if source.calls.Load() != 1 {
		t.Fatalf("source calls=%d", source.calls.Load())
	}
}

func TestCatalogReplayBoundsMeasurementLeadWithoutRefetch(t *testing.T) {
	const directoryCount = catalogReplayDirectoryTarget + 20
	share := transferID[catalog.ShareInstance](114)
	directories := make([]catalog.DirectoryID, directoryCount)
	snapshots := make(map[catalog.DirectoryID]catalog.DirectorySnapshot, directoryCount)
	for index := range directories {
		directory := replayDirectoryID(index)
		directories[index] = directory
		snapshots[directory] = jobSnapshot(t, share, directory, 1)
	}
	started := make(chan catalog.DirectoryID, directoryCount)
	source := &replayCatalogSource{snapshots: snapshots, started: started}
	replay := newCatalogReplay(source)
	measurement := replay.reader(catalogReplayMeasurement)
	execution := replay.reader(catalogReplayExecution)
	measurementDone := make(chan error, 1)
	go func() {
		for _, directory := range directories {
			if _, err := measurement.LoadDirectory(context.Background(), directory); err != nil {
				measurementDone <- err
				return
			}
			measurement.ReleaseDirectory(directory)
		}
		measurement.Close()
		measurementDone <- nil
	}()
	for range catalogReplayDirectoryTarget {
		<-started
	}

	for _, directory := range directories {
		if _, err := execution.LoadDirectory(context.Background(), directory); err != nil {
			t.Fatal(err)
		}
		execution.ReleaseDirectory(directory)
	}
	execution.Close()
	if err := <-measurementDone; err != nil {
		t.Fatal(err)
	}
	peakEntries, peakBytes, peakSingleEntryBytes := replay.peaks()
	if peakEntries > catalogReplayDirectoryTarget {
		t.Fatalf("peak replay entries=%d", peakEntries)
	}
	if peakBytes > catalogReplayMemoryTarget+peakSingleEntryBytes {
		t.Fatalf("peak replay bytes=%d", peakBytes)
	}
	if source.calls.Load() != directoryCount {
		t.Fatalf("source calls=%d want=%d", source.calls.Load(), directoryCount)
	}
	replay.mu.Lock()
	retained := len(replay.entries)
	replay.mu.Unlock()
	if retained != 0 {
		t.Fatalf("retained entries after both consumers closed=%d", retained)
	}
}

func TestCatalogReplayAllowsOneOversizedSnapshotWithoutLosingItsBound(t *testing.T) {
	const entryCount = 120_000
	share := transferID[catalog.ShareInstance](115)
	directory := replayDirectoryID(2_000)
	child := replayDirectoryID(2_001)
	generation := transferID[catalog.DirectoryGeneration](1)
	pages := make([]catalog.CatalogPage, 0, (entryCount+catalog.MaxCatalogPageEntries-1)/catalog.MaxCatalogPageEntries)
	var previous catalog.PageCommitment
	for first := 0; first < entryCount; first += catalog.MaxCatalogPageEntries {
		last := min(first+catalog.MaxCatalogPageEntries, entryCount)
		entries := make([]catalog.Entry, 0, last-first)
		for index := first; index < last; index++ {
			entry, err := catalog.NewFileEntry(replayFileID(index), fmt.Sprintf("file-%06d", index), 0, catalog.ModifiedTime{})
			if err != nil {
				t.Fatal(err)
			}
			entries = append(entries, entry)
		}
		page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
			ShareInstance: share, DirectoryID: directory, Generation: generation,
			PageIndex: uint32(len(pages)), Previous: previous, Entries: entries,
			Terminal: last == entryCount,
		}, jobPageCommitter{})
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, page)
		previous = page.Commitment()
	}
	snapshot, err := catalog.NewDirectorySnapshot(pages)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.EstimatedMemoryBytes() <= catalogReplayMemoryTarget {
		t.Fatalf("hostile snapshot estimate=%d", snapshot.EstimatedMemoryBytes())
	}
	source := &replayCatalogSource{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
		directory: snapshot,
		child:     jobSnapshot(t, share, child, 2),
	}}
	replay := newCatalogReplay(source)
	measurement := replay.reader(catalogReplayMeasurement)
	execution := replay.reader(catalogReplayExecution)
	if _, err := measurement.LoadDirectory(context.Background(), directory); err != nil {
		t.Fatal(err)
	}
	measurement.ReleaseDirectory(directory)
	childResult := make(chan error, 1)
	go func() {
		_, err := measurement.LoadDirectory(context.Background(), child)
		childResult <- err
	}()
	if _, err := execution.LoadDirectory(context.Background(), directory); err != nil {
		t.Fatal(err)
	}
	execution.ReleaseDirectory(directory)
	if err := <-childResult; err != nil {
		t.Fatalf("child after oversized parent: %v", err)
	}
	measurement.ReleaseDirectory(child)
	if _, err := execution.LoadDirectory(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	execution.ReleaseDirectory(child)
	measurement.Close()
	execution.Close()
	peakEntries, peakBytes, peakSingleEntryBytes := replay.peaks()
	if peakEntries != 1 || peakSingleEntryBytes != snapshot.EstimatedMemoryBytes() || peakBytes != peakSingleEntryBytes {
		t.Fatalf("oversized replay peak entries=%d bytes=%d single=%d", peakEntries, peakBytes, peakSingleEntryBytes)
	}
	if peakBytes > catalogReplayMemoryTarget+peakSingleEntryBytes {
		t.Fatalf("oversized replay escaped target+single bound: %d", peakBytes)
	}
	if source.calls.Load() != 2 {
		t.Fatalf("oversized source calls=%d", source.calls.Load())
	}
}

func TestCatalogReplayReleasesSourceLeaseBeforeNextBoundedLoad(t *testing.T) {
	share := transferID[catalog.ShareInstance](120)
	root := replayDirectoryID(3_000)
	child := replayDirectoryID(3_001)
	rootSnapshot := jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, child, "child"))
	childSnapshot := jobSnapshot(t, share, child, 2)
	wire := &jobCatalogWire{
		snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: rootSnapshot, child: childSnapshot},
		objects:   make(map[string]catalog.CatalogPage),
	}
	var maxCharge uint64
	for _, snapshot := range []catalog.DirectorySnapshot{rootSnapshot, childSnapshot} {
		page, _ := snapshot.Page(0)
		wire.objects[jobObjectKey(snapshot.DirectoryID(), 0)] = page
		charge := uint64(len(jobObjectKey(snapshot.DirectoryID(), 0))) + page.EstimatedMemoryBytes()
		maxCharge = max(maxCharge, charge)
	}
	client, err := catalogflow.NewClient(catalogflow.ClientConfig{
		ShareInstance: share, Transport: wire, Verifier: wire,
		MaxCacheBytes: maxCharge + catalogflow.CatalogLeaseClaimMemoryBytes, MaxDirectories: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	replay := newCatalogReplay(client)
	measurement := replay.reader(catalogReplayMeasurement)
	execution := replay.reader(catalogReplayExecution)
	for _, directory := range []catalog.DirectoryID{root, child} {
		if _, err := measurement.LoadDirectory(context.Background(), directory); err != nil {
			t.Fatal(err)
		}
		measurement.ReleaseDirectory(directory)
		if client.CachedBytes() != 0 {
			t.Fatalf("source bytes after %x=%d", directory, client.CachedBytes())
		}
	}
	for _, directory := range []catalog.DirectoryID{root, child} {
		if _, err := execution.LoadDirectory(context.Background(), directory); err != nil {
			t.Fatal(err)
		}
		execution.ReleaseDirectory(directory)
	}
	measurement.Close()
	execution.Close()
	replay.Wait()
	wire.mu.Lock()
	loads := len(wire.loads)
	wire.mu.Unlock()
	if loads != 2 {
		t.Fatalf("source loads=%d", loads)
	}
}
