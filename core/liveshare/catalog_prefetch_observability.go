package liveshare

import (
	"context"
	"encoding/hex"
	"log/slog"
	"reflect"

	"github.com/windshare/windshare/core/catalog"
)

const (
	rootPrefetchLogMessage          = "share: root prefetch"
	maximumRootPrefetchFailureNodes = 64
)

type structuredRootPrefetchTracer struct{ logger *slog.Logger }

func (decision RootPrefetchDecision) String() string {
	switch decision {
	case RootPrefetchAttemptStarted:
		return "attempt-started"
	case RootPrefetchYieldedToDemand:
		return "yielded-to-demand"
	case RootPrefetchRetryScheduled:
		return "retry-scheduled"
	case RootPrefetchCommitted:
		return "committed"
	case RootPrefetchBudgetFailed:
		return "budget-failed"
	case RootPrefetchScanFailed:
		return "scan-failed"
	case RootPrefetchStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

func rootPrefetchTracerOrDefault(tracer RootPrefetchTracer) RootPrefetchTracer {
	if tracer != nil {
		return tracer
	}
	return structuredRootPrefetchTracer{logger: slog.Default()}
}

func (tracer structuredRootPrefetchTracer) TraceRootPrefetch(event RootPrefetchTrace) {
	logger := tracer.logger
	if logger == nil {
		logger = slog.Default()
	}
	level := slog.LevelInfo
	if event.Decision == RootPrefetchBudgetFailed || event.Decision == RootPrefetchScanFailed {
		level = slog.LevelWarn
	}
	logger.LogAttrs(
		context.Background(), level, rootPrefetchLogMessage,
		slog.String("decision", event.Decision.String()),
		slog.String("share_instance", hex.EncodeToString(event.ShareInstance.Bytes())),
		slog.String("directory_id", hex.EncodeToString(event.DirectoryID.Bytes())),
		slog.String("generation", hex.EncodeToString(event.Generation.Bytes())),
		slog.Uint64("attempt", event.Attempt),
		slog.Uint64("entry_count", event.EntryCount),
		slog.Uint64("omitted_count", event.OmittedCount),
	)
}

func traceRootPrefetch(tracer RootPrefetchTracer, event RootPrefetchTrace) {
	if tracer == nil {
		return
	}
	// Optional diagnostics cannot gain authority over sender readiness or scans.
	defer func() { _ = recover() }()
	tracer.TraceRootPrefetch(event)
}

// rootPrefetchFailureDecision classifies only the one operational distinction
// needed by the trace contract. Bounding the collaborator-owned error graph
// prevents optional warm-up shutdown from hanging on a cyclic Unwrap chain;
// the recovery boundary likewise denies a faulty diagnostic unwrapper authority
// over sender availability.
//
//nolint:errorlint // Recursive errors.Is cannot provide a graph work bound.
func rootPrefetchFailureDecision(root error) (decision RootPrefetchDecision) {
	decision = RootPrefetchScanFailed
	defer func() {
		if recover() != nil {
			decision = RootPrefetchScanFailed
		}
	}()
	pending := []error{root}
	for inspected := 0; len(pending) != 0 && inspected < maximumRootPrefetchFailureNodes; inspected++ {
		last := len(pending) - 1
		current := pending[last]
		pending = pending[:last]
		if current == nil {
			continue
		}
		if reflect.TypeOf(current).Comparable() && current == catalog.ErrBudgetExceeded {
			return RootPrefetchBudgetFailed
		}
		remaining := maximumRootPrefetchFailureNodes - inspected - 1 - len(pending)
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if remaining > 0 {
				pending = append(pending, children[:min(len(children), remaining)]...)
			}
		case interface{ Unwrap() error }:
			if remaining > 0 {
				pending = append(pending, wrapped.Unwrap())
			}
		}
	}
	return decision
}
