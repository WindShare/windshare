package diaglog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
)

func listRotatingLogs(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, "host-") && strings.HasSuffix(name, ".jsonl") {
			names = append(names, name)
		}
	}
	return names
}

func TestRotatingRecorderJoinsFlushesAndRetainsBoundedFiles(t *testing.T) {
	directory := t.TempDir()
	for i := range retainedRotating + 2 {
		recorder, err := OpenRotating(directory, "session")
		if err != nil {
			t.Fatal(err)
		}
		recorder.Record("milestone", map[string]int{"task": i})
		recorder.Record("oversize", strings.Repeat("x", maximumRecordBytes+1))
		if err := recorder.Close(); err != nil {
			t.Fatal(err)
		}
		if err := recorder.Close(); err != nil {
			t.Fatal(err)
		}
		recorder.Record("after_close", nil)
	}
	files := listRotatingLogs(t, directory)
	if len(files) != retainedRotating {
		t.Fatalf("retention: %d", len(files))
	}
	data, err := os.ReadFile(filepath.Join(directory, files[len(files)-1]))
	if err != nil || !strings.Contains(string(data), "milestone") || !strings.Contains(string(data), `"dropped":1`) {
		t.Fatalf("flush: %s %v", data, err)
	}
}

func TestRotatingRecorderTreatsDirectoryLiterallyWithoutGlobExpansion(t *testing.T) {
	root := t.TempDir()

	unclosed := filepath.Join(root, "logs[unclosed")
	recorder, err := OpenRotating(unclosed, "unclosed")
	if err != nil {
		t.Fatalf("open unclosed bracket directory: %v", err)
	}
	recorder.Record("milestone", map[string]string{"case": "unclosed"})
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if files := listRotatingLogs(t, unclosed); len(files) != 1 {
		t.Fatalf("unclosed bracket log count = %d, want 1", len(files))
	}

	sibling := filepath.Join(root, "logs1")
	if err := os.MkdirAll(sibling, ownerOnlyDirectoryMode); err != nil {
		t.Fatal(err)
	}
	const siblingSeedCount = retainedRotating + 2
	for i := range siblingSeedCount {
		name := fmt.Sprintf("host-20200101T000000.000000000-sibling-%04d.jsonl", i+1)
		if err := os.WriteFile(filepath.Join(sibling, name), []byte("{}\n"), ownerOnlyFileMode); err != nil {
			t.Fatal(err)
		}
	}

	target := filepath.Join(root, "logs[1]")
	ignoredSubdir := filepath.Join(target, "host-20200101T000000.000000000-dir-0001.jsonl")
	if err := os.MkdirAll(ignoredSubdir, ownerOnlyDirectoryMode); err != nil {
		t.Fatal(err)
	}
	for i := range retainedRotating + 2 {
		rec, err := OpenRotating(target, fmt.Sprintf("literal-%02d", i))
		if err != nil {
			t.Fatalf("open bracketed directory: %v", err)
		}
		if err := rec.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if files := listRotatingLogs(t, target); len(files) != retainedRotating {
		t.Fatalf("bracketed directory retention = %d, want %d", len(files), retainedRotating)
	}
	if files := listRotatingLogs(t, sibling); len(files) != siblingSeedCount {
		t.Fatalf("sibling logs pruned across glob boundary: %d, want %d", len(files), siblingSeedCount)
	}
	if info, err := os.Stat(ignoredSubdir); err != nil || !info.IsDir() {
		t.Fatalf("matching subdirectory was removed or altered: info=%v err=%v", info, err)
	}
}

func TestRotatingRecorderRotatesAndReportsStorageErrors(t *testing.T) {
	recorder, err := OpenRotating(t.TempDir(), "rotation")
	if err != nil {
		t.Fatal(err)
	}
	recorder.size = maximumRotatingBytes
	recorder.Record("rotate", nil)
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if recorder.fileSequence != 2 {
		t.Fatal("size did not rotate")
	}
	if _, err := OpenRotating(filepath.Join(t.TempDir(), "bad\x00path"), "bad"); err == nil {
		t.Fatal("invalid storage accepted")
	}
	disabled := Disabled("session", errors.New("unavailable"))
	disabled.Record("ignored", nil)
	if err := disabled.Close(); err == nil {
		t.Fatal("disabled recorder lost startup error")
	}
}

func TestRunRecorderWritesSequencedJSONLAndSummaryWithSingleDroppedCounter(t *testing.T) {
	dir := t.TempDir()
	exactPath := filepath.Join(dir, "trace.ndjson")
	target, err := ExactFile(exactPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	recorder, err := OpenRun(target, "share", RunOptions{
		Now: func() time.Time {
			current := now
			now = now.Add(15 * time.Millisecond)
			return current
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if recorder.Path() != exactPath || len(recorder.RunID()) != 22 {
		t.Fatalf("path=%q runID=%q", recorder.Path(), recorder.RunID())
	}
	if _, err := OpenRun(target, "share", RunOptions{}); !errors.Is(err, ErrTraceExists) {
		t.Fatalf("exclusive collision = %v, want ErrTraceExists", err)
	}

	scope := diagnostics.Scope{EngineID: "eng", TaskID: "eng:1", Workflow: "share"}
	if !recorder.RecordRun(RunRecord{
		Level:      "info",
		Event:      "ready",
		Scope:      &scope,
		SourceTime: time.Date(2026, 11, 22, 12, 0, 0, 5e6, time.UTC),
		Payload:    map[string]any{"ok": true},
	}) {
		t.Fatal("failed to enqueue run event")
	}
	recorder.AddDropped(2)
	status := recorder.CloseRun()
	if status.EventsWritten != 1 || status.Dropped != 2 || status.WriterFailed || status.Complete() {
		t.Fatalf("unexpected run status: %+v", status)
	}
	if recorder.RecordRun(RunRecord{Event: "after_close"}) {
		t.Fatal("accepted event after close")
	}

	raw, err := os.ReadFile(exactPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("lines=%d, want 2:\n%s", len(lines), raw)
	}
	var first, summary map[string]any
	if err := json.Unmarshal(lines[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lines[1], &summary); err != nil {
		t.Fatal(err)
	}
	if first["event"] != "ready" || first["sequence"] != "1" || first["command"] != "share" {
		t.Fatalf("unexpected first record: %+v", first)
	}
	if summary["event"] != "trace_summary" || summary["level"] != "warn" {
		t.Fatalf("unexpected summary record: %+v", summary)
	}
	summaryPayload := summary["payload"].(map[string]any)
	if summaryPayload["dropped"] != "2" || summaryPayload["events_written"] != "1" || summaryPayload["incomplete"] != true {
		t.Fatalf("unexpected summary payload: %+v", summaryPayload)
	}
}

func TestRunDirectoryGeneratesUniqueFilesAndValidatesTargets(t *testing.T) {
	if _, err := ExactFile(""); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("empty exact = %v", err)
	}
	if _, err := ExactFile("-"); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("stdout exact = %v", err)
	}
	if _, err := RunDirectory("-"); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("stdout dir = %v", err)
	}
	dir := filepath.Join(t.TempDir(), "traces")
	target, err := RunDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := OpenRun(target, "get", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(recorder.Path()), "get-") || !strings.HasSuffix(recorder.Path(), ".ndjson") {
		t.Fatalf("generated path = %q", recorder.Path())
	}
	status := recorder.CloseRun()
	if !status.Complete() || status.EventsWritten != 0 {
		t.Fatalf("status = %+v", status)
	}
}
