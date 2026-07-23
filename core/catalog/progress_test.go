package catalog

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Coverage and race instrumentation make the 256-entry publish boundary much
// slower than production. Synchronization channels prove the ordering; this
// deadline only prevents a genuinely stuck test from hanging the suite.
const scanProgressTestDeadline = 30 * time.Second

func TestScanProgressCoalescesBackpressureWithoutBlockingScan(t *testing.T) {
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	defer store.Close()
	session := generousBudget(t, "progress-session")
	directory := prepareScannableDirectory(t, store, session, 181, 183)

	observerStarted := make(chan struct{})
	releaseObserver := make(chan struct{})
	milestonePublished := make(chan struct{})
	var startOnce sync.Once
	var updatesMu sync.Mutex
	var updates []ScanProgress
	observer := ScanProgressObserverFunc(func(ctx context.Context, progress ScanProgress) error {
		updatesMu.Lock()
		updates = append(updates, progress)
		updatesMu.Unlock()
		if progress.DiscoveredEntries == 1 {
			startOnce.Do(func() { close(observerStarted) })
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-releaseObserver:
			}
		}
		return nil
	})
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		for index := range 300 {
			if err := request.Children.Add(ctx, progressScannedFile(t, index)); err != nil {
				return ScanResult{}, err
			}
			if index == 0 {
				select {
				case <-ctx.Done():
					return ScanResult{}, ctx.Err()
				case <-observerStarted:
				}
			}
			if index+1 == int(ScanProgressEntryInterval) {
				close(milestonePublished)
			}
		}
		return ScanResult{}, nil
	})
	result := make(chan error, 1)
	go func() {
		_, err := store.ListChildren(context.Background(), directory, session, ScanOptions{Progress: observer}, scanner)
		result <- err
	}()

	select {
	case <-milestonePublished:
		// The observer is still blocked on the first milestone. Returning from
		// Add at the next publish boundary proves receiver backpressure did not
		// serialize the filesystem scan; completion/commit cost is irrelevant.
	case <-time.After(scanProgressTestDeadline):
		t.Fatal("scan was blocked by a progress observer")
	}
	close(releaseObserver)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	updatesMu.Lock()
	defer updatesMu.Unlock()
	if len(updates) < 2 || updates[0].DiscoveredEntries != 1 ||
		updates[len(updates)-1].DiscoveredEntries != 300 {
		t.Fatalf("coalesced milestones = %+v", updates)
	}
	for index := 1; index < len(updates); index++ {
		if updates[index].AttemptID != updates[0].AttemptID ||
			updates[index].DiscoveredEntries <= updates[index-1].DiscoveredEntries {
			t.Fatalf("progress changed attempt or regressed: %+v", updates)
		}
	}
}

func TestScanProgressCancellationStopsLastSharedAttempt(t *testing.T) {
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	defer store.Close()
	session := generousBudget(t, "progress-cancel-session")
	directory := prepareScannableDirectory(t, store, session, 184, 186)
	observerStarted := make(chan struct{})
	scannerStopped := make(chan struct{})
	observer := ScanProgressObserverFunc(func(ctx context.Context, _ ScanProgress) error {
		close(observerStarted)
		<-ctx.Done()
		return ctx.Err()
	})
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		if err := request.Children.Add(ctx, progressScannedFile(t, 1)); err != nil {
			return ScanResult{}, err
		}
		<-ctx.Done()
		close(scannerStopped)
		return ScanResult{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := store.ListChildren(ctx, directory, session, ScanOptions{Progress: observer}, scanner)
		result <- err
	}()
	<-observerStarted
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled progress waiter = %v", err)
	}
	select {
	case <-scannerStopped:
	case <-time.After(scanProgressTestDeadline):
		t.Fatal("last progress waiter cancellation left scan running")
	}
}

func TestScanProgressObserverFailureIsWaiterLocalAndNeverPersisted(t *testing.T) {
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	defer store.Close()
	session := generousBudget(t, "progress-failure-session")
	directory := prepareScannableDirectory(t, store, session, 187, 189)
	started := make(chan struct{})
	release := make(chan struct{})
	var scans int
	var scansMu sync.Mutex
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		scansMu.Lock()
		scans++
		call := scans
		scansMu.Unlock()
		if call == 1 {
			close(started)
			select {
			case <-ctx.Done():
				return ScanResult{}, ctx.Err()
			case <-release:
			}
		}
		if err := request.Children.Add(ctx, progressScannedFile(t, call)); err != nil {
			return ScanResult{}, err
		}
		return ScanResult{}, nil
	})
	wantObserverErr := errors.New("progress writer failed")
	failureResult := make(chan error, 1)
	failedAttempt := make(chan ScanAttemptID, 1)
	go func() {
		_, err := store.ListChildren(
			context.Background(), directory, session,
			ScanOptions{Progress: ScanProgressObserverFunc(func(_ context.Context, progress ScanProgress) error {
				failedAttempt <- progress.AttemptID
				return wantObserverErr
			})},
			scanner,
		)
		failureResult <- err
	}()
	<-started

	successUpdates := make(chan ScanProgress, 2)
	successResult := make(chan error, 1)
	go func() {
		_, err := store.ListChildren(
			context.Background(), directory, session,
			ScanOptions{Progress: ScanProgressObserverFunc(func(_ context.Context, progress ScanProgress) error {
				successUpdates <- progress
				return nil
			})},
			scanner,
		)
		successResult <- err
	}()
	waitForScanWaiters(t, store, directory, 2)
	close(release)
	if err := <-failureResult; !errors.Is(err, wantObserverErr) {
		t.Fatalf("failed observer result = %v", err)
	}
	if err := <-successResult; err != nil {
		t.Fatalf("healthy shared waiter failed: %v", err)
	}
	failedID := <-failedAttempt
	healthy := <-successUpdates
	if failedID != healthy.AttemptID {
		t.Fatalf("singleflight observers saw different attempts: %x / %x", failedID, healthy.AttemptID)
	}
	if _, found, err := store.FailureObject(context.Background(), directory, failedID); err != nil || found {
		t.Fatalf("observer failure became directory failure authority: found=%v err=%v", found, err)
	}
	scansMu.Lock()
	defer scansMu.Unlock()
	if scans != 1 {
		t.Fatalf("observer failure restarted shared scan %d times", scans)
	}
}

func waitForScanWaiters(
	t *testing.T,
	store *CatalogStore,
	directory DirectoryID,
	want int,
) {
	t.Helper()
	deadline := time.NewTimer(scanProgressTestDeadline)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		store.mu.Lock()
		attempt := store.attempts[directory]
		got := 0
		if attempt != nil {
			got = attempt.waiters
		}
		store.mu.Unlock()
		if got >= want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("scan waiters = %d, want at least %d", got, want)
		case <-ticker.C:
		}
	}
}

func progressScannedFile(t *testing.T, index int) ScannedChild {
	t.Helper()
	name := fmt.Sprintf("progress-%06d", index)
	child := scannedFile(t, byte(index%250+1), name, uint64(index+1))
	var file FileID
	binary.BigEndian.PutUint64(file[IdentityBytes/2:], uint64(index+1))
	child.FileID = file
	return child
}
