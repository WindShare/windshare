package runtrace

import (
	"encoding/json"
	"sync/atomic"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
)

type traceContent struct {
	file           TraceFile
	completeOffset int64
	durableOffset  int64
	unsynced       bool
	boundaryIntact bool
}

func newTraceContent(file TraceFile) *traceContent {
	return &traceContent{file: file, boundaryIntact: true}
}

func (content *traceContent) appendRecord(data []byte) bool {
	if !content.boundaryIntact {
		return false
	}
	recordStart := content.completeOffset
	written, err := content.file.Write(data)
	if err == nil && written == len(data) {
		content.completeOffset += int64(written)
		content.unsynced = true
		return true
	}
	// A failed append is not a record. Returning to the prior newline keeps every
	// retained byte independently decodable before any best-effort summary append.
	content.unsynced = true
	if rollbackErr := content.file.Rollback(recordStart); rollbackErr != nil {
		content.boundaryIntact = false
		return false
	}
	return false
}

func (content *traceContent) rollback(offset int64) bool {
	if !content.boundaryIntact || offset < 0 || offset > content.completeOffset {
		return false
	}
	if err := content.file.Rollback(offset); err != nil {
		content.boundaryIntact = false
		return false
	}
	content.completeOffset = offset
	content.unsynced = true
	return true
}

func (content *traceContent) sync() bool {
	if !content.boundaryIntact {
		return false
	}
	if err := content.file.Flush(); err != nil {
		return false
	}
	content.durableOffset = content.completeOffset
	content.unsynced = false
	return true
}

func (content *traceContent) needsSync() bool {
	return content.unsynced || content.completeOffset != content.durableOffset
}

func (recorder *Recorder) writeLoop() {
	tick := recorder.ticker.C()
	content := newTraceContent(recorder.file)
	defer func() {
		recorder.ticker.Stop()
		// Final sync is the content-authority cut. Close only releases the handle,
		// so a cleanup failure cannot retroactively falsify the durable summary.
		_ = recorder.file.Close()
		close(recorder.health)
		close(recorder.done)
	}()

	for {
		select {
		case queued := <-recorder.lifecycle:
			recorder.writeLifecycle(content, queued)
		case _, ok := <-tick:
			if !ok {
				tick = nil
				continue
			}
			recorder.writeReadySnapshot(content)
			if content.needsSync() {
				recorder.syncContent(content)
			}
		case <-recorder.closing:
			recorder.finish(content)
			return
		}
	}
}

func (recorder *Recorder) finish(content *traceContent) {
	recorder.writeReadySnapshot(content)
	if content.needsSync() {
		recorder.syncContent(content)
	}
	recorder.writeSummary(content)
}

func (recorder *Recorder) writeReadySnapshot(content *traceContent) {
	lifecycle, progress := recorder.snapshotReady()
	for _, queued := range lifecycle {
		if progress != nil && progress.metadata.sequence < queued.metadata.sequence {
			recorder.writeNormal(content, *progress)
			progress = nil
		}
		recorder.writeNormal(content, queued)
	}
	if progress != nil {
		recorder.writeNormal(content, *progress)
	}
}

func (recorder *Recorder) snapshotReady() ([]queuedEvent, *queuedEvent) {
	// Record publishes lifecycle and progress while holding entryMu. Taking both
	// queues at the same cut prevents a newer progress sample from overtaking an
	// older lifecycle event without putting file IO on producer goroutines.
	recorder.entryMu.Lock()
	defer recorder.entryMu.Unlock()
	lifecycle := make([]queuedEvent, 0, len(recorder.lifecycle))
	for {
		select {
		case queued := <-recorder.lifecycle:
			lifecycle = append(lifecycle, queued)
		default:
			return lifecycle, recorder.takeProgress()
		}
	}
}

func (recorder *Recorder) writeLifecycle(content *traceContent, queued queuedEvent) {
	if progress := recorder.takeProgressBefore(queued.metadata.sequence); progress != nil {
		recorder.writeNormal(content, *progress)
	}
	recorder.writeNormal(content, queued)
}

func (recorder *Recorder) takeProgress() *queuedEvent {
	recorder.progressMu.Lock()
	defer recorder.progressMu.Unlock()
	progress := recorder.progress
	recorder.progress = nil
	return progress
}

func (recorder *Recorder) takeProgressBefore(sequence uint64) *queuedEvent {
	recorder.progressMu.Lock()
	defer recorder.progressMu.Unlock()
	if recorder.progress == nil || recorder.progress.metadata.sequence >= sequence {
		return nil
	}
	progress := recorder.progress
	recorder.progress = nil
	return progress
}

func (recorder *Recorder) writeNormal(content *traceContent, queued queuedEvent) {
	if recorder.disabled.Load() {
		recorder.countDropped(queued.progress, 1)
		return
	}
	record, err := encodeV2(recorder.runID, queued.metadata, queued.event)
	if err != nil {
		recorder.countDropped(queued.progress, 1)
		recorder.schemaLimited.Store(true)
		recorder.disabled.Store(true)
		recorder.markIncomplete(clievent.TraceIncompleteSchemaLimit)
		return
	}
	data, err := json.Marshal(record)
	if err != nil {
		recorder.countDropped(queued.progress, 1)
		recorder.schemaLimited.Store(true)
		recorder.disabled.Store(true)
		recorder.markIncomplete(clievent.TraceIncompleteSchemaLimit)
		return
	}
	data = append(data, '\n')
	if !content.appendRecord(data) {
		recorder.countDropped(queued.progress, 1)
		recorder.writerFailed.Store(true)
		recorder.disabled.Store(true)
		recorder.markIncomplete(clievent.TraceIncompleteWriter)
		return
	}
	recorder.addCounter(&recorder.eventsWritten, 1)
}

func (recorder *Recorder) syncContent(content *traceContent) bool {
	if content.sync() {
		return true
	}
	recorder.flushFailed.Store(true)
	recorder.disabled.Store(true)
	recorder.markIncomplete(clievent.TraceIncompleteFlush)
	return false
}

func (recorder *Recorder) writeSummary(content *traceContent) {
	if !recorder.hasSummaryMetadata {
		recorder.syncContent(content)
		return
	}
	// Only failures known before encoding belong in the summary. If the append or
	// authority sync fails, the summary is removed instead of claiming stale health.
	record := summaryV2(recorder.runID, recorder.command, recorder.summaryMetadata, recorder.Status())
	data, err := json.Marshal(record)
	if err != nil {
		recorder.schemaLimited.Store(true)
		recorder.markIncomplete(clievent.TraceIncompleteSchemaLimit)
		recorder.syncContent(content)
		return
	}
	data = append(data, '\n')
	summaryOffset := content.completeOffset
	if !content.appendRecord(data) {
		recorder.writerFailed.Store(true)
		recorder.markIncomplete(clievent.TraceIncompleteWriter)
		recorder.syncContent(content)
		return
	}
	if recorder.syncContent(content) {
		return
	}
	if !content.rollback(summaryOffset) {
		recorder.writerFailed.Store(true)
		return
	}
	// Persisting the rollback is best effort. Completeness remains false because
	// the first final sync failed even when this recovery sync succeeds.
	recorder.syncContent(content)
}

func (recorder *Recorder) countDropped(progress bool, count uint64) {
	if progress {
		recorder.addProgressDropped(count)
		return
	}
	recorder.addLifecycleDropped(count)
}

func (recorder *Recorder) addLifecycleDropped(count uint64) {
	recorder.addCounter(&recorder.lifecycleDropped, count)
}

func (recorder *Recorder) addProgressDropped(count uint64) {
	recorder.addCounter(&recorder.progressDropped, count)
}

func (recorder *Recorder) addCounter(counter *atomic.Uint64, increment uint64) {
	if increment == 0 {
		return
	}
	for {
		current := counter.Load()
		if ^uint64(0)-current < increment {
			if counter.CompareAndSwap(current, ^uint64(0)) {
				recorder.schemaLimited.Store(true)
				recorder.markIncomplete(clievent.TraceIncompleteSchemaLimit)
				return
			}
			continue
		}
		if counter.CompareAndSwap(current, current+increment) {
			return
		}
	}
}

func (recorder *Recorder) markIncomplete(cause clievent.TraceIncompleteCause) {
	recorder.healthOnce.Do(func() {
		health, err := clievent.NewTraceIncomplete(
			recorder.command,
			cause,
			recorder.lifecycleDropped.Load(),
			recorder.progressDropped.Load(),
		)
		if err == nil {
			recorder.health <- health
		}
	})
}
