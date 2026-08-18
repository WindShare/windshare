package cli

import (
	"sync/atomic"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
)

type pendingRuntimeLoss struct {
	lifecycle uint64
	progress  uint64
}

func (runtime *commandRuntime) reportPendingLoss() {
	runtime.entryMu.Lock()
	loss := runtime.collectPendingLossLocked()
	runtime.entryMu.Unlock()
	loss.lifecycle = saturatingAdd(loss.lifecycle, runtime.pendingTraceLoss.Swap(0))
	loss.progress = saturatingAdd(loss.progress, runtime.pendingTraceProgress.Swap(0))
	runtime.reportUpstreamLoss(loss)
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

func (runtime *commandRuntime) collectPendingLossLocked() pendingRuntimeLoss {
	loss := pendingRuntimeLoss{progress: runtime.pendingProgressLoss.Swap(0)}
	for category := clievent.ObserverLossCategory(1); category < clievent.ObserverLossCategoryLimit; category++ {
		for reason := clievent.ObserverLossReason(1); reason < clievent.ObserverLossReasonLimit; reason++ {
			count := runtime.pendingObserverLoss[category][reason].Swap(0)
			if count == 0 {
				continue
			}
			loss.lifecycle = saturatingAdd(loss.lifecycle, count)
			if runtime.trace == nil || runtime.entrySequence == ^uint64(0) {
				continue
			}
			event, err := clievent.NewObserverLossObserved(clievent.ObserverLossSpec{
				Command: runtime.command, Category: category, Reason: reason, Count: count,
			})
			if err != nil {
				continue
			}
			runtime.entrySequence++
			runtime.commandPublications = append(runtime.commandPublications, queuedCommandEvent{
				sequence: runtime.entrySequence, event: event,
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
