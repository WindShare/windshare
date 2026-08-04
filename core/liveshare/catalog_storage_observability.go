package liveshare

import (
	"context"
	"encoding/hex"
	"log/slog"
)

const catalogStorageLogMessage = "share: catalog storage"

type structuredCatalogStorageTracer struct {
	logger *slog.Logger
}

func catalogStorageTracerOrDefault(tracer CatalogStorageTracer) CatalogStorageTracer {
	if tracer != nil {
		return tracer
	}
	// Catalog recovery runs before the application can assemble a session runtime.
	// A process-wide slog handler keeps these required diagnostics observable while
	// allowing an embedding application to own formatting and destination.
	return structuredCatalogStorageTracer{logger: slog.Default()}
}

func (tracer structuredCatalogStorageTracer) TraceCatalogStorage(event CatalogStorageTrace) {
	level := slog.LevelInfo
	if event.Failed {
		level = slog.LevelError
	}
	cause := "-"
	if event.Cause != nil {
		cause = event.Cause.Error()
	}
	tracer.logger.LogAttrs(
		context.Background(),
		level,
		catalogStorageLogMessage,
		slog.String("operation", event.Operation.String()),
		slog.String("share_instance", hex.EncodeToString(event.ShareInstance.Bytes())),
		slog.Uint64("recovered_entries", event.RecoveredUsage.Entries),
		slog.Uint64("recovered_memory_bytes", event.RecoveredUsage.MemoryBytes),
		slog.Uint64("recovered_spill_bytes", event.RecoveredUsage.SpillBytes),
		slog.Uint64("legacy_roots_removed", event.LegacyRootsRemoved),
		slog.Bool("failed", event.Failed),
		slog.String("cause", cause),
	)
}
