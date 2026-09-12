package cli

import (
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
)

const (
	maxProjectionRejectionSignatures  = 32
	projectionRejectionReportInterval = 5 * time.Second
)

type pendingProjectionRejection struct {
	category   clievent.ObserverLossCategory
	reason     clievent.ObserverLossReason
	sample     clievent.ObservationRejection
	count      uint64
	reported   bool
	lastReport time.Time
}

// The table is allocated only on failure. IDs are samples, not signature keys,
// so retries and new sessions cannot grow its memory or reset rate limiting.
func (runtime *commandRuntime) ReportObservationRejection(category clievent.ObserverLossCategory, reason clievent.ObserverLossReason, sample clievent.ObservationRejection) bool {
	if runtime == nil {
		return false
	}
	runtime.entryMu.Lock()
	defer runtime.entryMu.Unlock()
	if runtime.closed {
		return false
	}
	runtime.unreportedRejections = saturatingAdd(runtime.unreportedRejections, 1)
	_, reasonOK := reason.Name()
	_, categoryOK := category.Name()
	if !reasonOK || !categoryOK || !sample.Valid() {
		runtime.loseRejectionEvidenceLocked(1)
		runtime.signalReadyLocked()
		return false
	}
	if runtime.trace != nil {
		runtime.recordProjectionRejectionLocked(category, reason, sample)
	}
	runtime.signalReadyLocked()
	return true
}

func (runtime *commandRuntime) recordProjectionRejectionLocked(category clievent.ObserverLossCategory, reason clievent.ObserverLossReason, sample clievent.ObservationRejection) {
	for i := range runtime.projectionRejections {
		entry := &runtime.projectionRejections[i]
		if entry.category == category && entry.reason == reason &&
			entry.sample.Event == sample.Event && entry.sample.Source == sample.Source &&
			entry.sample.Stage == sample.Stage && entry.sample.Field == sample.Field && entry.sample.Rule == sample.Rule {
			entry.count = saturatingAdd(entry.count, 1)
			return
		}
	}
	if len(runtime.projectionRejections) == maxProjectionRejectionSignatures {
		entry := &runtime.projectionRejections[maxProjectionRejectionSignatures-1]
		entry.count = saturatingAdd(entry.count, 1)
		return
	}
	if len(runtime.projectionRejections) == maxProjectionRejectionSignatures-1 {
		// The final slot accounts for additional signatures without evicting
		// the original samples or allocating unbounded keys.
		category, reason = clievent.ObserverLossCommandAdapter, clievent.ObserverLossEventContract
		sample = clievent.ObservationRejection{
			Event: "observation_rejection", Source: "cli.commandRuntime",
			Stage: "overflow", Field: "rejection_signatures", Rule: "detail_capacity_exceeded",
		}
	}
	sample = clievent.CaptureObservationRejection(sample, sample.Evidence()...)
	runtime.projectionRejections = append(runtime.projectionRejections, pendingProjectionRejection{
		category: category, reason: reason, sample: sample, count: 1,
	})
}

func (runtime *commandRuntime) collectProjectionRejectionsLocked(final bool) uint64 {
	count := runtime.unreportedRejections
	runtime.unreportedRejections = 0
	for i := range runtime.projectionRejections {
		entry := &runtime.projectionRejections[i]
		if entry.count == 0 {
			continue
		}
		now := runtime.clock.Now()
		if !final && entry.reported && now.Sub(entry.lastReport) < projectionRejectionReportInterval {
			continue
		}
		var omittedSamples uint64
		if i == maxProjectionRejectionSignatures-1 {
			omittedSamples = entry.count
		}
		runtime.enqueueObserverLossLocked(clievent.ObserverLossSpec{
			Command: runtime.command, Category: entry.category, Reason: entry.reason,
			Count: entry.count, Rejection: entry.sample, OmittedSamples: omittedSamples,
		})
		entry.count, entry.reported, entry.lastReport = 0, true, now
	}
	return count
}

func (runtime *commandRuntime) enqueueObserverLossLocked(spec clievent.ObserverLossSpec) {
	if runtime.trace == nil {
		return
	}
	if runtime.entrySequence == ^uint64(0) {
		if spec.Rejection != (clievent.ObservationRejection{}) {
			runtime.loseRejectionEvidenceLocked(spec.Count)
		}
		return
	}
	event, err := clievent.NewObserverLossObserved(spec)
	if err != nil {
		if spec.Rejection != (clievent.ObservationRejection{}) {
			runtime.loseRejectionEvidenceLocked(spec.Count)
		}
		return
	}
	runtime.entrySequence++
	runtime.commandPublications = append(runtime.commandPublications, queuedCommandEvent{
		sequence: runtime.entrySequence, event: event,
	})
	runtime.signalReadyLocked()
}

// Evidence loss uses an independent numeric path: a failed anomaly must never
// construct another anomaly or count the original observation loss twice.
func (runtime *commandRuntime) loseRejectionEvidenceLocked(count uint64) {
	if runtime.trace == nil {
		return
	}
	saturatingAtomicAdd(&runtime.rejectionEvidenceDropped, count)
	saturatingAtomicAdd(&runtime.pendingRejectionEvidenceLoss, count)
}

func (runtime *commandRuntime) reportRejectionEvidenceLoss() {
	if runtime.trace == nil {
		return
	}
	count := runtime.pendingRejectionEvidenceLoss.Swap(0)
	if count == 0 {
		return
	}
	if !runtime.trace.ReportRejectionEvidenceLoss(count) {
		saturatingAtomicAdd(&runtime.pendingRejectionEvidenceLoss, count)
		runtime.rejectionEvidenceLossReportFailed.Store(true)
	}
}

type pendingRuntimeLoss struct {
	lifecycle uint64
	progress  uint64
}

func (runtime *commandRuntime) detailedDiagnosticsEnabled() bool {
	return runtime != nil && runtime.detailedDiagnostics
}

func (runtime *commandRuntime) traceRecordingEnabled() bool {
	return runtime != nil && runtime.trace != nil
}

// ReportObserverLoss accounts for facts dropped by a bounded producer adapter
// before they could be offered to Observe. Recorder-local Record failures must not
// be reported here because runtrace already owns those counters.
func (runtime *commandRuntime) ReportObserverLoss(category clievent.ObserverLossCategory, reason clievent.ObserverLossReason, count uint64) bool {
	if runtime == nil {
		return false
	}
	if _, ok := category.Name(); !ok {
		return false
	}
	if _, ok := reason.Name(); !ok || count == 0 {
		return false
	}
	runtime.entryMu.Lock()
	defer runtime.entryMu.Unlock()
	if runtime.closed {
		return false
	}
	runtime.addObserverLoss(category, reason, count)
	runtime.signalReadyLocked()
	return true
}

func (runtime *commandRuntime) ReportCumulativeObserverLoss(category clievent.ObserverLossCategory, reason clievent.ObserverLossReason, cumulative uint64) bool {
	if runtime == nil || cumulative == 0 {
		return false
	}
	if _, ok := category.Name(); !ok {
		return false
	}
	if _, ok := reason.Name(); !ok {
		return false
	}
	runtime.entryMu.Lock()
	defer runtime.entryMu.Unlock()
	if runtime.closed {
		return false
	}
	counter := &runtime.upstreamCumulative[category][reason]
	previous := counter.Load()
	if cumulative <= previous {
		return true
	}
	counter.Store(cumulative)
	runtime.addObserverLoss(category, reason, cumulative-previous)
	runtime.signalReadyLocked()
	return true
}

func (runtime *commandRuntime) addObserverLoss(category clievent.ObserverLossCategory, reason clievent.ObserverLossReason, count uint64) {
	if category > 0 && category < clievent.ObserverLossCategoryLimit && reason > 0 && reason < clievent.ObserverLossReasonLimit {
		saturatingAtomicAdd(&runtime.pendingObserverLoss[category][reason], count)
	}
}

func (runtime *commandRuntime) HumanOutputError() error {
	if runtime == nil || runtime.canvas == nil {
		return nil
	}
	return runtime.canvas.Err()
}

func (runtime *commandRuntime) traceHealth() <-chan clievent.TraceIncomplete {
	if runtime.trace == nil {
		return nil
	}
	return runtime.trace.Health()
}

func (runtime *commandRuntime) reportPendingLoss() {
	runtime.entryMu.Lock()
	loss := runtime.collectPendingLossLocked(runtime.closed || runtime.observerFinalized)
	runtime.entryMu.Unlock()
	loss.lifecycle = saturatingAdd(loss.lifecycle, runtime.pendingTraceLoss.Swap(0))
	loss.progress = saturatingAdd(loss.progress, runtime.pendingTraceProgress.Swap(0))
	runtime.reportUpstreamLoss(loss)
	runtime.reportRejectionEvidenceLoss()
}

func (runtime *commandRuntime) scheduleUpstreamLossLocked(loss pendingRuntimeLoss) {
	if loss.lifecycle != 0 {
		saturatingAtomicAdd(&runtime.pendingTraceLoss, loss.lifecycle)
	}
	if loss.progress != 0 {
		saturatingAtomicAdd(&runtime.pendingTraceProgress, loss.progress)
	}
	if loss.lifecycle != 0 || loss.progress != 0 {
		runtime.signalReadyLocked()
	}
}

func (runtime *commandRuntime) collectPendingLossLocked(final bool) pendingRuntimeLoss {
	loss := pendingRuntimeLoss{
		progress:  runtime.pendingProgressLoss.Swap(0),
		lifecycle: runtime.collectProjectionRejectionsLocked(final),
	}
	for category := clievent.ObserverLossCategory(1); category < clievent.ObserverLossCategoryLimit; category++ {
		for reason := clievent.ObserverLossReason(1); reason < clievent.ObserverLossReasonLimit; reason++ {
			count := runtime.pendingObserverLoss[category][reason].Swap(0)
			if count == 0 {
				continue
			}
			loss.lifecycle = saturatingAdd(loss.lifecycle, count)
			runtime.enqueueObserverLossLocked(clievent.ObserverLossSpec{
				Command: runtime.command, Category: category, Reason: reason, Count: count,
			})
		}
	}
	if loss.lifecycle != 0 {
		runtime.signalReadyLocked()
	}
	return loss
}

func (runtime *commandRuntime) reportUpstreamLoss(loss pendingRuntimeLoss) {
	if runtime.trace == nil || (loss.lifecycle == 0 && loss.progress == 0) {
		return
	}
	_ = runtime.trace.ReportUpstreamLoss(loss.lifecycle, loss.progress)
	if loss.lifecycle != 0 {
		runtime.warnTraceIncomplete(traceIncompleteFromStatus(runtime.command, runtrace.Status{LifecycleDropped: loss.lifecycle}))
	}
}

func (runtime *commandRuntime) drainTraceHealth() {
	health := runtime.trace.Health()
	for {
		select {
		case event, open := <-health:
			if !open {
				return
			}
			runtime.warnTraceIncomplete(event)
		default:
			return
		}
	}
}

func (runtime *commandRuntime) warnTraceIncomplete(event clievent.TraceIncomplete) {
	runtime.warningOnce.Do(func() {
		_ = runtime.human.Render(event)
	})
}

func traceIncompleteFromStatus(command clievent.Command, status runtrace.Status) clievent.TraceIncomplete {
	cause := clievent.TraceIncompleteLifecycleDrop
	switch {
	case status.WriterFailed:
		cause = clievent.TraceIncompleteWriter
	case status.FlushFailed:
		cause = clievent.TraceIncompleteFlush
	case status.SchemaLimited:
		cause = clievent.TraceIncompleteSchemaLimit
	case status.RejectionEvidenceDropped != 0:
		cause = clievent.TraceIncompleteRejectionEvidence
	case status.LifecycleDropped == 0:
		cause = clievent.TraceIncompleteWriter
	}
	event, err := clievent.NewTraceIncomplete(
		command, cause, status.LifecycleDropped, status.ProgressDropped,
	)
	if err == nil {
		return event
	}
	// The fallback is constructible for every valid command and keeps an
	// inconsistent recorder status from leaking provider text into stderr.
	event, _ = clievent.NewTraceIncomplete(command, clievent.TraceIncompleteWriter, 0, 0)
	return event
}

func saturatingAtomicAdd(counter *atomic.Uint64, amount uint64) {
	if amount == 0 {
		return
	}
	for {
		current := counter.Load()
		next := current + amount
		if next < current {
			next = ^uint64(0)
		}
		if counter.CompareAndSwap(current, next) {
			return
		}
	}
}

func saturatingAdd(current, amount uint64) uint64 {
	next := current + amount
	if next < current {
		return ^uint64(0)
	}
	return next
}
