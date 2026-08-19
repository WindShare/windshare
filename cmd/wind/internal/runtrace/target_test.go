package runtrace

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestTargetConstructorsRejectInvalidNamespacesBeforeOpening(t *testing.T) {
	for name, construct := range map[string]func(string) (Target, error){
		"exact file":    ExactFile,
		"run directory": RunDirectory,
	} {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{"", "-"} {
				target, err := construct(path)
				if !errors.Is(err, ErrInvalidTarget) || target != (Target{}) {
					t.Fatalf("construct %q = target %+v, err %v", path, target, err)
				}
			}
		})
	}

	openCalls := 0
	_, err := OpenWithDependencies(Target{kind: targetKind(255), path: "trace"}, clievent.CommandShare, Config{}, Dependencies{
		OpenFile: func(string) (TraceFile, error) {
			openCalls++
			return &memoryTraceFile{}, nil
		},
	})
	if !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("unknown target error = %v", err)
	}
	if openCalls != 0 {
		t.Fatalf("invalid target made %d open calls", openCalls)
	}
}

func TestExactTargetCollisionIsOneFailFastAttempt(t *testing.T) {
	ticker := newManualTicker()
	openCalls := 0
	recorder, err := OpenWithDependencies(mustExactTarget(t, "evidence.ndjson"), clievent.CommandShare, Config{}, Dependencies{
		Clock:  fixedClock(),
		Random: bytes.NewReader(bytes.Repeat([]byte{1}, clievent.IdentityBytes)),
		EnsureDirectory: func(string) error {
			t.Fatal("exact target attempted to prepare a directory")
			return nil
		},
		OpenFile: func(path string) (TraceFile, error) {
			openCalls++
			return nil, fs.ErrExist
		},
		NewTicker: func(time.Duration) Ticker { return ticker },
	})
	if recorder != nil || !errors.Is(err, ErrTraceExists) {
		t.Fatalf("exact collision = recorder %v, err %v", recorder, err)
	}
	if openCalls != 1 {
		t.Fatalf("exact collision made %d open attempts, want 1", openCalls)
	}
	awaitSignal(t, ticker.stopped, "ticker stop after exact collision")
}

func TestDirectoryTargetRetriesFreshRunIdentities(t *testing.T) {
	started := time.Date(2026, 8, 17, 10, 34, 56, 123456789, time.FixedZone("test", 8*60*60))
	clock := &incrementingClock{current: started, step: time.Millisecond}
	ticker := newManualTicker()
	file := &memoryTraceFile{}
	var opened []string
	prepareCalls := 0
	recorder, err := OpenWithDependencies(mustRunDirectory(t, "trace-root"), clievent.CommandGet, Config{}, Dependencies{
		Clock:  clock,
		Random: identitySequence(1, 2, 3),
		EnsureDirectory: func(path string) error {
			prepareCalls++
			if path != "trace-root" {
				t.Fatalf("prepared directory = %q", path)
			}
			return nil
		},
		OpenFile: func(path string) (TraceFile, error) {
			if prepareCalls != 1 {
				t.Fatalf("open attempted after %d directory preparations", prepareCalls)
			}
			opened = append(opened, path)
			if len(opened) < 3 {
				return nil, ErrTraceExists
			}
			return file, nil
		},
		NewTicker: func(time.Duration) Ticker { return ticker },
	})
	if err != nil {
		t.Fatal(err)
	}
	wantRunID := repeatedRunID(3)
	wantPath := filepath.Join(
		"trace-root",
		"get-20260817T023456Z-"+repeatedRunIdentity(3).filenameToken()+".ndjson",
	)
	if recorder.RunID() != wantRunID || recorder.Path() != wantPath {
		t.Fatalf("identity = (%q, %q), want (%q, %q)", recorder.RunID(), recorder.Path(), wantRunID, wantPath)
	}
	if len(opened) != 3 || opened[2] != wantPath {
		t.Fatalf("opened paths = %q", opened)
	}
	if prepareCalls != 1 {
		t.Fatalf("directory preparations = %d, want 1", prepareCalls)
	}
	for index, path := range opened {
		wantToken := repeatedRunIdentity(byte(index + 1)).filenameToken()
		if !strings.HasSuffix(path, "-"+wantToken+".ndjson") {
			t.Fatalf("attempt %d path %q does not carry filename token %q", index+1, path, wantToken)
		}
	}
	if status := recorder.Close(); !status.Complete {
		t.Fatalf("close status: %+v", status)
	}
	records := decodeRecords(t, file.Bytes())
	if len(records) != 1 || records[0].RunID != wantRunID {
		t.Fatalf("trace records do not retain full run identity: %+v", records)
	}
}

func TestDirectoryTargetPreparationFailureStopsBeforeIdentityAndFileClaim(t *testing.T) {
	ticker := newManualTicker()
	prepareCalls := 0
	openCalls := 0
	recorder, err := OpenWithDependencies(mustRunDirectory(t, "trace-root"), clievent.CommandGet, Config{}, Dependencies{
		Clock:  fixedClock(),
		Random: bytes.NewReader(nil),
		EnsureDirectory: func(path string) error {
			prepareCalls++
			if path != "trace-root" {
				t.Fatalf("prepared directory = %q", path)
			}
			return errInjected
		},
		OpenFile: func(string) (TraceFile, error) {
			openCalls++
			return &memoryTraceFile{}, nil
		},
		NewTicker: func(time.Duration) Ticker { return ticker },
	})
	if recorder != nil || !errors.Is(err, ErrTraceDirectoryUnavailable) {
		t.Fatalf("directory preparation failure = recorder %v, err %v", recorder, err)
	}
	if prepareCalls != 1 || openCalls != 0 {
		t.Fatalf("directory preparations = %d, file claims = %d", prepareCalls, openCalls)
	}
	awaitSignal(t, ticker.stopped, "ticker stop after directory preparation failure")
}

func TestDirectoryTargetCollisionExhaustionPreservesEverySibling(t *testing.T) {
	directory := t.TempDir()
	started := time.Date(2026, 8, 17, 2, 34, 56, 0, time.UTC)
	randomValues := make([]byte, 0, directoryCreateAttempts*clievent.IdentityBytes)
	wantContents := make(map[string]string, directoryCreateAttempts)
	for attempt := 1; attempt <= directoryCreateAttempts; attempt++ {
		value := byte(attempt)
		randomValues = append(randomValues, bytes.Repeat([]byte{value}, clievent.IdentityBytes)...)
		path := directoryTracePath(directory, clievent.CommandShare, started, repeatedRunIdentity(value))
		contents := "retained evidence " + repeatedRunID(value)
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		wantContents[path] = contents
	}
	ticker := newManualTicker()
	recorder, err := OpenWithDependencies(mustRunDirectory(t, directory), clievent.CommandShare, Config{}, Dependencies{
		Clock:     &incrementingClock{current: started},
		Random:    bytes.NewReader(randomValues),
		NewTicker: func(time.Duration) Ticker { return ticker },
	})
	if recorder != nil || !errors.Is(err, ErrTraceNameUnavailable) || errors.Is(err, ErrTraceExists) {
		t.Fatalf("collision exhaustion = recorder %v, err %v", recorder, err)
	}
	awaitSignal(t, ticker.stopped, "ticker stop after collision exhaustion")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != directoryCreateAttempts {
		t.Fatalf("directory entries = %d, want %d", len(entries), directoryCreateAttempts)
	}
	for path, want := range wantContents {
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != want {
			t.Fatalf("sibling %q changed: %q, err %v", path, contents, err)
		}
	}
}

func TestDirectoryTargetDoesNotRetryNonCollisionFailure(t *testing.T) {
	openCalls := 0
	ticker := newManualTicker()
	recorder, err := OpenWithDependencies(mustRunDirectory(t, "trace-root"), clievent.CommandGet, Config{}, Dependencies{
		Clock:           fixedClock(),
		Random:          identitySequence(1, 2),
		EnsureDirectory: func(string) error { return nil },
		OpenFile: func(string) (TraceFile, error) {
			openCalls++
			return nil, errInjected
		},
		NewTicker: func(time.Duration) Ticker { return ticker },
	})
	if recorder != nil || !errors.Is(err, ErrTraceFileUnavailable) {
		t.Fatalf("directory open failure = recorder %v, err %v", recorder, err)
	}
	if openCalls != 1 {
		t.Fatalf("non-collision failure made %d attempts, want 1", openCalls)
	}
	awaitSignal(t, ticker.stopped, "ticker stop after directory failure")
}

func TestDirectoryTargetCreatesCompactOwnerOnlyTraceWithFullRunIdentity(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "new", "nested", "traces")
	started := time.Date(2026, 8, 17, 10, 34, 56, 123456789, time.FixedZone("test", 8*60*60))
	recorder, err := OpenWithDependencies(mustRunDirectory(t, directory), clievent.CommandGet, Config{}, Dependencies{
		Clock:  &incrementingClock{current: started, step: time.Millisecond},
		Random: identitySequence(9),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantRunID := repeatedRunID(9)
	wantName := "get-20260817T023456Z-" + repeatedRunIdentity(9).filenameToken() + ".ndjson"
	if recorder.Path() != filepath.Join(directory, wantName) || recorder.RunID() != wantRunID {
		t.Fatalf("created trace = path %q, run ID %q", recorder.Path(), recorder.RunID())
	}
	if status := recorder.Close(); !status.Complete {
		t.Fatalf("close status: %+v", status)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != wantName {
		t.Fatalf("created entries = %+v", entries)
	}
	if runtime.GOOS != "windows" {
		directoryInfo, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if directoryInfo.Mode().Perm() != ownerOnlyDirectoryMode {
			t.Fatalf("trace directory mode = %o, want %o", directoryInfo.Mode().Perm(), ownerOnlyDirectoryMode)
		}
		info, err := entries[0].Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != ownerOnlyFileMode {
			t.Fatalf("trace mode = %o, want %o", info.Mode().Perm(), ownerOnlyFileMode)
		}
	}
	contents, err := os.ReadFile(recorder.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(contents, []byte(directory)) {
		t.Fatal("local trace directory leaked into NDJSON")
	}
	records := decodeRecords(t, contents)
	if len(records) != 1 || records[0].RunID != wantRunID {
		t.Fatalf("trace records = %+v", records)
	}
}

func TestDirectoryTargetConcurrentNativeOpensOwnDistinctFiles(t *testing.T) {
	const recorderCount = 32
	directory := filepath.Join(t.TempDir(), "concurrent", "traces")
	target := mustRunDirectory(t, directory)
	recorders := make(chan *Recorder, recorderCount)
	errorsSeen := make(chan error, recorderCount)
	var wait sync.WaitGroup
	for range recorderCount {
		wait.Go(func() {
			recorder, err := Open(target, clievent.CommandShare, Config{})
			if err != nil {
				errorsSeen <- err
				return
			}
			recorders <- recorder
		})
	}
	wait.Wait()
	close(recorders)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("concurrent open: %v", err)
	}
	paths := make(map[string]struct{}, recorderCount)
	opened := 0
	for recorder := range recorders {
		opened++
		if _, duplicate := paths[recorder.Path()]; duplicate {
			t.Errorf("duplicate recorder path %q", recorder.Path())
		}
		paths[recorder.Path()] = struct{}{}
		filenameToken := recorder.runID.filenameToken()
		if !strings.HasSuffix(filepath.Base(recorder.Path()), "-"+filenameToken+".ndjson") {
			t.Errorf("path %q does not carry filename token %q", recorder.Path(), filenameToken)
		}
		if status := recorder.Close(); !status.Complete {
			t.Errorf("close %q: %+v", recorder.Path(), status)
		}
	}
	if opened != recorderCount {
		t.Fatalf("successful recorders = %d, want %d", opened, recorderCount)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != recorderCount {
		t.Fatalf("native directory entries = %d, want %d", len(entries), recorderCount)
	}
}

func identitySequence(values ...byte) *bytes.Reader {
	raw := make([]byte, 0, len(values)*clievent.IdentityBytes)
	for _, value := range values {
		raw = append(raw, bytes.Repeat([]byte{value}, clievent.IdentityBytes)...)
	}
	return bytes.NewReader(raw)
}

func repeatedRunIdentity(value byte) runIdentity {
	var identity runIdentity
	copy(identity[:], bytes.Repeat([]byte{value}, clievent.IdentityBytes))
	return identity
}

func repeatedRunID(value byte) string {
	return repeatedRunIdentity(value).encoded()
}
