package liveshare

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

type rootPrefetchLogRecord struct {
	Level         string `json:"level"`
	Message       string `json:"msg"`
	Decision      string `json:"decision"`
	ShareInstance string `json:"share_instance"`
	DirectoryID   string `json:"directory_id"`
	Generation    string `json:"generation"`
	Attempt       uint64 `json:"attempt"`
	EntryCount    uint64 `json:"entry_count"`
	OmittedCount  uint64 `json:"omitted_count"`
}

func TestStructuredRootPrefetchTracerPreservesPrivacySafeDecisionContext(t *testing.T) {
	share := catalogAccessShare(t, 71)
	directory := catalogAccessDirectory(t, 72)
	generation := catalogAccessGeneration(t, 73)
	events := []RootPrefetchTrace{
		{
			Decision: RootPrefetchCommitted, ShareInstance: share, DirectoryID: directory,
			Generation: generation, Attempt: 2, EntryCount: 5, OmittedCount: 3,
		},
		{
			Decision: RootPrefetchBudgetFailed, ShareInstance: share, DirectoryID: directory, Attempt: 3,
		},
	}
	var output bytes.Buffer
	tracer := structuredRootPrefetchTracer{logger: slog.New(slog.NewJSONHandler(&output, nil))}
	for _, event := range events {
		tracer.TraceRootPrefetch(event)
	}

	decoder := json.NewDecoder(&output)
	var records []rootPrefetchLogRecord
	for {
		var record rootPrefetchLogRecord
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if len(records) != 2 {
		t.Fatalf("structured prefetch records = %#v", records)
	}
	wantShare := hex.EncodeToString(share.Bytes())
	wantDirectory := hex.EncodeToString(directory.Bytes())
	if records[0] != (rootPrefetchLogRecord{
		Level: "INFO", Message: rootPrefetchLogMessage, Decision: "committed",
		ShareInstance: wantShare, DirectoryID: wantDirectory,
		Generation: hex.EncodeToString(generation.Bytes()), Attempt: 2, EntryCount: 5, OmittedCount: 3,
	}) {
		t.Fatalf("committed prefetch record = %#v", records[0])
	}
	if records[1] != (rootPrefetchLogRecord{
		Level: "WARN", Message: rootPrefetchLogMessage, Decision: "budget-failed",
		ShareInstance: wantShare, DirectoryID: wantDirectory,
		Generation: hex.EncodeToString(catalog.DirectoryGeneration{}.Bytes()), Attempt: 3,
	}) {
		t.Fatalf("failed prefetch record = %#v", records[1])
	}
}

func TestRootPrefetchTracerDefaultsWithoutReplacingExplicitObserver(t *testing.T) {
	if _, ok := rootPrefetchTracerOrDefault(nil).(structuredRootPrefetchTracer); !ok {
		t.Fatalf("default root prefetch tracer = %T", rootPrefetchTracerOrDefault(nil))
	}
	called := false
	explicit := RootPrefetchTraceFunc(func(RootPrefetchTrace) { called = true })
	rootPrefetchTracerOrDefault(explicit).TraceRootPrefetch(RootPrefetchTrace{})
	if !called {
		t.Fatal("explicit root prefetch tracer was replaced")
	}
	if got := RootPrefetchDecision(255).String(); got != "unknown" {
		t.Fatalf("unknown root prefetch decision = %q", got)
	}
}

func catalogAccessGeneration(t *testing.T, seed byte) catalog.DirectoryGeneration {
	t.Helper()
	value := make([]byte, catalog.IdentityBytes)
	for index := range value {
		value[index] = seed + byte(index)
	}
	generation, err := catalog.DirectoryGenerationFromBytes(value)
	if err != nil {
		t.Fatal(err)
	}
	return generation
}
