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

type catalogStorageLogRecord struct {
	Level                string `json:"level"`
	Message              string `json:"msg"`
	Operation            string `json:"operation"`
	ShareInstance        string `json:"share_instance"`
	RecoveredEntries     uint64 `json:"recovered_entries"`
	RecoveredMemoryBytes uint64 `json:"recovered_memory_bytes"`
	RecoveredSpillBytes  uint64 `json:"recovered_spill_bytes"`
	LegacyRootsRemoved   uint64 `json:"legacy_roots_removed"`
	Failed               bool   `json:"failed"`
	Cause                string `json:"cause"`
}

func TestStructuredCatalogStorageTracerPreservesMilestoneIdentityAndDecisionContext(t *testing.T) {
	share := catalogStorageTestShare(31)
	failure := errors.New("catalog recovery failed")
	events := []CatalogStorageTrace{
		{
			Operation: CatalogStorageCleaned, ShareInstance: share,
			LegacyRootsRemoved: 3,
		},
		{
			Operation: CatalogStorageRecovered, ShareInstance: share,
			RecoveredUsage: catalog.ResourceUsage{Entries: 5, MemoryBytes: 7, SpillBytes: 11},
			Failed:         true, Cause: failure,
		},
	}

	var output bytes.Buffer
	tracer := structuredCatalogStorageTracer{
		logger: slog.New(slog.NewJSONHandler(&output, nil)),
	}
	for _, event := range events {
		tracer.TraceCatalogStorage(event)
	}

	decoder := json.NewDecoder(&output)
	var records []catalogStorageLogRecord
	for {
		var record catalogStorageLogRecord
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
		t.Fatalf("structured catalog records = %#v", records)
	}
	wantShare := hex.EncodeToString(share.Bytes())
	if records[0] != (catalogStorageLogRecord{
		Level: "INFO", Message: catalogStorageLogMessage, Operation: "cleaned", ShareInstance: wantShare,
		LegacyRootsRemoved: 3, Cause: "-",
	}) {
		t.Fatalf("successful cleanup record = %#v", records[0])
	}
	if records[1] != (catalogStorageLogRecord{
		Level: "ERROR", Message: catalogStorageLogMessage, Operation: "recovered", ShareInstance: wantShare,
		RecoveredEntries: 5, RecoveredMemoryBytes: 7, RecoveredSpillBytes: 11,
		Failed: true, Cause: failure.Error(),
	}) {
		t.Fatalf("failed recovery record = %#v", records[1])
	}
}

func TestCatalogStorageTracerDefaultsWithoutReplacingExplicitObserver(t *testing.T) {
	if _, ok := catalogStorageTracerOrDefault(nil).(structuredCatalogStorageTracer); !ok {
		t.Fatalf("default catalog tracer = %T", catalogStorageTracerOrDefault(nil))
	}
	called := false
	explicit := CatalogStorageTraceFunc(func(CatalogStorageTrace) { called = true })
	selected := catalogStorageTracerOrDefault(explicit)
	selected.TraceCatalogStorage(CatalogStorageTrace{})
	if !called {
		t.Fatal("explicit catalog tracer was replaced")
	}
}
