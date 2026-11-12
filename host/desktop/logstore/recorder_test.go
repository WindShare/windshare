package logstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecorderJoinsFlushesAndRetainsBoundedFiles(t *testing.T) {
	directory := t.TempDir()
	for i := range retainedFiles + 2 {
		recorder, err := Open(directory, "session")
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
	files, err := filepath.Glob(filepath.Join(directory, "host-*.jsonl"))
	if err != nil || len(files) != retainedFiles {
		t.Fatalf("retention: %d %v", len(files), err)
	}
	data, err := os.ReadFile(files[len(files)-1])
	if err != nil || !strings.Contains(string(data), "milestone") || !strings.Contains(string(data), "\"dropped\":1") {
		t.Fatalf("flush: %s %v", data, err)
	}
}

func TestRecorderRotatesAndReportsStorageErrors(t *testing.T) {
	recorder, err := Open(t.TempDir(), "rotation")
	if err != nil {
		t.Fatal(err)
	}
	recorder.size = maximumFileBytes
	recorder.Record("rotate", nil)
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if recorder.sequence != 2 {
		t.Fatal("size did not rotate")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "bad\x00path"), "bad"); err == nil {
		t.Fatal("invalid storage accepted")
	}
}
