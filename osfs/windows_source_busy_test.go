//go:build windows

package osfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	diag "github.com/windshare/windshare/core/diagnostics"
	"golang.org/x/sys/windows"
)

func setNoDelayRetryWait(source *RootedRevisionSource) {
	noWait := func(context.Context, time.Duration) error { return nil }
	source.wait = noWait
	if binder, ok := source.binder.(*WindowsStabilityBinder); ok {
		binder.wait = noWait
	}
}

type scriptedWindowsRevisionRoot struct {
	mu       sync.Mutex
	openFn   func(relative string, attempt int) (windowsRevisionFile, error)
	attempts map[string]int
	closed   int
}

func (r *scriptedWindowsRevisionRoot) Identity() ([windowsRevisionIdentityBytes]byte, error) {
	return [windowsRevisionIdentityBytes]byte{}, nil
}

func (r *scriptedWindowsRevisionRoot) OpenStable(relative string) (windowsRevisionFile, error) {
	r.mu.Lock()
	if r.attempts == nil {
		r.attempts = make(map[string]int)
	}
	r.attempts[relative]++
	attempt := r.attempts[relative]
	fn := r.openFn
	r.mu.Unlock()
	return fn(relative, attempt)
}

func (r *scriptedWindowsRevisionRoot) Close() error {
	r.mu.Lock()
	r.closed++
	r.mu.Unlock()
	return nil
}

func collectRetryTraces(stream *diag.Stream) []diag.Record {
	stream.Close()
	var records []diag.Record
	for record := range stream.Records() {
		if record.Name() == "revision_open_sharing_retry" {
			records = append(records, record)
		}
	}
	return records
}

func TestWindowsStabilityBinderRecoversFromTransientSharingViolation(t *testing.T) {
	baseline := windowsTestToken(3, 4)
	stableHandle := &fakeWindowsRevisionFile{tokens: []windowsMutationToken{baseline}, data: []byte("data")}
	root := &scriptedWindowsRevisionRoot{
		openFn: func(_ string, attempt int) (windowsRevisionFile, error) {
			if attempt <= 2 {
				return nil, classifyWindowsStableOpenError(windows.ERROR_SHARING_VIOLATION)
			}
			return stableHandle, nil
		},
	}
	platform := &fakeWindowsRevisionPlatform{tokens: []windowsMutationToken{baseline, baseline}, root: nil}
	stream, err := diag.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	binder := &WindowsStabilityBinder{
		platform:    platform,
		roots:       []windowsRevisionRoot{root},
		diagnostics: stream.Sink(),
		wait: func(_ context.Context, delay time.Duration) error {
			waits = append(waits, delay)
			return nil
		},
	}
	defer binder.Close()

	preliminary := windowsPreliminaryFile(t)
	defer preliminary.Close()
	stable, err := binder.BindStable(context.Background(), StableBinding{
		File: preliminary, Record: windowsTestRecord(t, baseline, "brief.bin"), RelativePath: "brief.bin",
	})
	if err != nil {
		t.Fatalf("BindStable after brief sharing violation = %v, want nil", err)
	}
	defer stable.Close()

	if !slices.Equal(waits, revisionOpenSharingBackoff[:2]) {
		t.Fatalf("waits = %v, want %v", waits, revisionOpenSharingBackoff[:2])
	}
	if got := root.attempts["brief.bin"]; got != 3 {
		t.Fatalf("open attempts = %d, want 3", got)
	}
	records := collectRetryTraces(stream)
	if len(records) != 1 {
		t.Fatalf("retry trace count = %d, want 1", len(records))
	}
	payload := records[0].Payload()
	if payload.Get("stage").Text() != "stable" ||
		payload.Get("outcome").Text() != "recovered" ||
		payload.Get("attempts").Uint() != 3 {
		t.Fatalf("unexpected retry trace payload: %+v", payload)
	}
}

func TestWindowsStabilityBinderRejectsFileMutatedDuringBackoff(t *testing.T) {
	baseline := windowsTestToken(4, 4)
	mutated := baseline
	mutated.lastWrite += 10_000_000
	stableHandle := &fakeWindowsRevisionFile{tokens: []windowsMutationToken{mutated}, data: []byte("edit")}
	root := &scriptedWindowsRevisionRoot{
		openFn: func(_ string, attempt int) (windowsRevisionFile, error) {
			if attempt == 1 {
				return nil, classifyWindowsStableOpenError(windows.ERROR_SHARING_VIOLATION)
			}
			return stableHandle, nil
		},
	}
	platform := &fakeWindowsRevisionPlatform{tokens: []windowsMutationToken{baseline, mutated}}
	binder := &WindowsStabilityBinder{
		platform: platform,
		roots:    []windowsRevisionRoot{root},
		wait:     func(context.Context, time.Duration) error { return nil },
	}
	defer binder.Close()

	preliminary := windowsPreliminaryFile(t)
	defer preliminary.Close()
	stable, err := binder.BindStable(context.Background(), StableBinding{
		File: preliminary, Record: windowsTestRecord(t, baseline, "mutated.bin"), RelativePath: "mutated.bin",
	})
	if stable != nil || !errors.Is(err, content.ErrRevisionStale) ||
		content.RevisionComparisonOf(err) != content.RevisionComparisonMismatch {
		if stable != nil {
			_ = stable.Close()
		}
		t.Fatalf("BindStable after mutation during backoff = (%v, %v), want ErrRevisionStale/mismatch", stable, err)
	}
	if stableHandle.closed != 1 {
		t.Fatalf("mutated stable handle closes = %d, want 1", stableHandle.closed)
	}
}

func TestWindowsStabilityBinderCancelsBackoffImmediatelyOnContextDone(t *testing.T) {
	baseline := windowsTestToken(5, 4)
	root := &scriptedWindowsRevisionRoot{
		openFn: func(string, int) (windowsRevisionFile, error) {
			return nil, classifyWindowsStableOpenError(windows.ERROR_SHARING_VIOLATION)
		},
	}
	platform := &fakeWindowsRevisionPlatform{tokens: []windowsMutationToken{baseline}}
	stream, err := diag.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	binder := &WindowsStabilityBinder{
		platform:    platform,
		roots:       []windowsRevisionRoot{root},
		diagnostics: stream.Sink(),
		wait: func(waitCtx context.Context, _ time.Duration) error {
			cancel()
			return waitCtx.Err()
		},
	}
	defer binder.Close()

	preliminary := windowsPreliminaryFile(t)
	defer preliminary.Close()
	_, err = binder.BindStable(ctx, StableBinding{
		File: preliminary, Record: windowsTestRecord(t, baseline, "cancel.bin"), RelativePath: "cancel.bin",
	})
	if !errors.Is(err, context.Canceled) || content.RevisionComparisonOf(err) != content.RevisionComparisonUnavailable {
		t.Fatalf("BindStable on canceled backoff = %v, want context.Canceled", err)
	}
	if got := root.attempts["cancel.bin"]; got != 1 {
		t.Fatalf("open attempts = %d, want 1", got)
	}
	records := collectRetryTraces(stream)
	if len(records) != 1 || records[0].Payload().Get("outcome").Text() != "canceled" {
		t.Fatalf("unexpected retry traces: %+v", records)
	}
}

func TestWindowsStabilityBinderReturnsSourceBusyWhenBackoffExhausted(t *testing.T) {
	baseline := windowsTestToken(6, 4)
	root := &scriptedWindowsRevisionRoot{
		openFn: func(string, int) (windowsRevisionFile, error) {
			return nil, classifyWindowsStableOpenError(windows.ERROR_SHARING_VIOLATION)
		},
	}
	platform := &fakeWindowsRevisionPlatform{tokens: []windowsMutationToken{baseline}}
	stream, err := diag.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	binder := &WindowsStabilityBinder{
		platform:    platform,
		roots:       []windowsRevisionRoot{root},
		diagnostics: stream.Sink(),
		wait: func(_ context.Context, delay time.Duration) error {
			waits = append(waits, delay)
			return nil
		},
	}
	defer binder.Close()

	preliminary := windowsPreliminaryFile(t)
	defer preliminary.Close()
	_, err = binder.BindStable(context.Background(), StableBinding{
		File: preliminary, Record: windowsTestRecord(t, baseline, "locked.bin"), RelativePath: "locked.bin",
	})
	if !errors.Is(err, content.ErrSourceBusy) || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, content.ErrUnsupportedStability) ||
		content.RevisionComparisonOf(err) != content.RevisionComparisonUnavailable {
		t.Fatalf("BindStable exhausted = %v, want ErrSourceBusy + ERROR_SHARING_VIOLATION", err)
	}
	if !slices.Equal(waits, revisionOpenSharingBackoff[:]) {
		t.Fatalf("waits = %v, want %v", waits, revisionOpenSharingBackoff[:])
	}
	if wantAttempts := 1 + len(revisionOpenSharingBackoff); root.attempts["locked.bin"] != wantAttempts {
		t.Fatalf("open attempts = %d, want %d", root.attempts["locked.bin"], wantAttempts)
	}
	records := collectRetryTraces(stream)
	if len(records) != 1 || records[0].Payload().Get("outcome").Text() != "exhausted" ||
		records[0].Payload().Get("attempts").Uint() != uint64(1+len(revisionOpenSharingBackoff)) {
		t.Fatalf("unexpected retry traces: %+v", records)
	}
}

func TestWindowsStabilityBinderBackoffWaitDoesNotBlockSiblingOrClose(t *testing.T) {
	baseline := windowsTestToken(7, 4)
	siblingHandle := &fakeWindowsRevisionFile{tokens: []windowsMutationToken{baseline}, data: []byte("data")}
	root := &scriptedWindowsRevisionRoot{
		openFn: func(relative string, _ int) (windowsRevisionFile, error) {
			if relative == "busy.bin" {
				return nil, classifyWindowsStableOpenError(windows.ERROR_SHARING_VIOLATION)
			}
			return siblingHandle, nil
		},
	}
	platform := &fakeWindowsRevisionPlatform{tokens: []windowsMutationToken{baseline, baseline, baseline}}
	inWait := make(chan struct{})
	releaseWait := make(chan struct{})
	binder := &WindowsStabilityBinder{
		platform: platform,
		roots:    []windowsRevisionRoot{root},
		wait: func(context.Context, time.Duration) error {
			close(inWait)
			<-releaseWait
			return nil
		},
	}

	busyPrelim := windowsPreliminaryFile(t)
	defer busyPrelim.Close()
	busyDone := make(chan error, 1)
	go func() {
		_, err := binder.BindStable(context.Background(), StableBinding{
			File: busyPrelim, Record: windowsTestRecord(t, baseline, "busy.bin"), RelativePath: "busy.bin",
		})
		busyDone <- err
	}()
	<-inWait

	siblingPrelim := windowsPreliminaryFile(t)
	defer siblingPrelim.Close()
	sibling, err := binder.BindStable(context.Background(), StableBinding{
		File: siblingPrelim, Record: windowsTestRecord(t, baseline, "healthy.bin"), RelativePath: "healthy.bin",
	})
	if err != nil {
		close(releaseWait)
		t.Fatalf("sibling BindStable while peer waiting = %v, want nil", err)
	}
	_ = sibling.Close()

	closeDone := make(chan error, 1)
	go func() { closeDone <- binder.Close() }()
	select {
	case closeErr := <-closeDone:
		if closeErr != nil {
			close(releaseWait)
			t.Fatalf("binder.Close while peer waiting = %v, want nil", closeErr)
		}
	case <-time.After(time.Second):
		close(releaseWait)
		t.Fatal("binder.Close blocked on a sleeping BindStable retry")
	}

	close(releaseWait)
	if busyErr := <-busyDone; !errors.Is(busyErr, content.ErrRevisionStoreClosed) {
		t.Fatalf("resumed busy BindStable after Close = %v, want ErrRevisionStoreClosed", busyErr)
	}
}

func TestWindowsRootedRevisionSourceRetriesExclusivePreliminaryOpen(t *testing.T) {
	classified := classifyRootRelativeOpenError("open stable revision", "locked.bin", windows.ERROR_SHARING_VIOLATION)
	if !errors.Is(classified, content.ErrSourceBusy) || !errors.Is(classified, windows.ERROR_SHARING_VIOLATION) ||
		content.RevisionComparisonOf(classified) != content.RevisionComparisonUnavailable {
		t.Fatalf("classifyRootRelativeOpenError(ERROR_SHARING_VIOLATION) = %v", classified)
	}

	root := t.TempDir()
	path := filepath.Join(root, "exclusive.bin")
	payload := []byte("data")
	if err := os.WriteFile(path, payload, windowsStabilityTestFilePermission); err != nil {
		t.Fatal(err)
	}
	baselineFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, candidate, err := WindowsCatalogBaseline(baselineFile)
	_ = baselineFile.Close()
	if errors.Is(err, content.ErrUnsupportedStability) {
		t.Skipf("Windows test provider lacks a required source capability: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}

	var fileID catalog.FileID
	fileID[0] = 9
	var parent catalog.DirectoryID
	parent[0] = 10
	locator, _ := NewSourceReference(0, "exclusive.bin")
	record, err := catalog.NewFileNodeRecord(fileID, parent, "exclusive.bin", locator, identity, candidate, uint64(len(payload)), catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}

	source, err := NewWindowsRootedRevisionSource([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Share mode 0 denies even FILE_SHARE_READ, exercising the preliminary
	// openRootObjectNoRecall backoff path before BindStable runs.
	exclusiveHandle, err := windows.CreateFile(
		path16, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			_ = windows.CloseHandle(exclusiveHandle)
		}
	}()

	var waits []time.Duration
	source.wait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		if len(waits) == 2 && !released {
			released = true
			return windows.CloseHandle(exclusiveHandle)
		}
		return nil
	}
	if binder, ok := source.binder.(*WindowsStabilityBinder); ok {
		binder.wait = func(context.Context, time.Duration) error { return nil }
	}

	stable, err := source.OpenStable(context.Background(), record)
	if err != nil {
		t.Fatalf("OpenStable after brief exclusive lock = %v, want nil", err)
	}
	defer stable.Close()
	if !slices.Equal(waits, revisionOpenSharingBackoff[:2]) {
		t.Fatalf("preliminary waits = %v, want %v", waits, revisionOpenSharingBackoff[:2])
	}
	buf := make([]byte, len(payload))
	if n, readErr := stable.ReadAt(context.Background(), buf, 0); readErr != nil || n != len(payload) || !bytes.Equal(buf, payload) {
		t.Fatalf("ReadAt = (%d, %q, %v), want %q", n, buf, readErr, payload)
	}
}
