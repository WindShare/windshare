package transfer

import (
	"math"
	"sync"

	"github.com/windshare/windshare/core/content"
)

type ConnectionSizeClass uint8

const (
	ConnectionSizeUnknown ConnectionSizeClass = iota
	ConnectionSizeSmall
	ConnectionSizeLarge
)

type DiscoveryStatus uint8

const (
	DiscoveryOpen DiscoveryStatus = iota + 1
	DiscoveryComplete
	DiscoveryFailed
)

// ReceiveProgressSnapshot contains only authenticated receive facts. Clocks,
// rates, and presentation policy remain outside core so polling cannot affect
// transfer authority or introduce I/O on the data path.
type ReceiveProgressSnapshot struct {
	DiscoveredFiles    uint64
	DiscoveredBytes    uint64
	PublishedFiles     uint64
	PublishedBytes     uint64
	VerifiedBytes      uint64
	NewlyVerifiedBytes uint64
	FileOutcomes       FileOutcomeSummary
	Discovery          DiscoveryStatus
	CountersExact      bool
}

func (snapshot ReceiveProgressSnapshot) ConnectionSizeClass() ConnectionSizeClass {
	if !snapshot.CountersExact || snapshot.DiscoveredFiles >= SmallTransferFileLimit ||
		snapshot.DiscoveredBytes >= SmallTransferByteLimit {
		return ConnectionSizeLarge
	}
	if snapshot.Discovery == DiscoveryComplete {
		return ConnectionSizeSmall
	}
	return ConnectionSizeUnknown
}

type discoveredSelection struct {
	files uint64
	bytes uint64
	exact bool
}

func newDiscoveredSelection() discoveredSelection {
	return discoveredSelection{exact: true}
}

func (selection *discoveredSelection) addFile(size uint64) {
	selection.files, selection.exact = saturatingAdd(selection.files, 1, selection.exact)
	selection.bytes, selection.exact = saturatingAdd(selection.bytes, size, selection.exact)
}

type receiveProgressTracker struct {
	mu       sync.RWMutex
	snapshot ReceiveProgressSnapshot
	updates  chan ReceiveProgressSnapshot
	closed   bool
}

func newReceiveProgressTracker() receiveProgressTracker {
	initial := ReceiveProgressSnapshot{Discovery: DiscoveryOpen, CountersExact: true}
	updates := make(chan ReceiveProgressSnapshot, 1)
	updates <- initial
	return receiveProgressTracker{snapshot: initial, updates: updates}
}

func (tracker *receiveProgressTracker) addDiscovery(selection discoveredSelection) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.snapshot.Discovery == DiscoveryComplete {
		return
	}
	tracker.snapshot.DiscoveredFiles, tracker.snapshot.CountersExact = saturatingAdd(
		tracker.snapshot.DiscoveredFiles, selection.files, tracker.snapshot.CountersExact,
	)
	tracker.snapshot.DiscoveredBytes, tracker.snapshot.CountersExact = saturatingAdd(
		tracker.snapshot.DiscoveredBytes, selection.bytes, tracker.snapshot.CountersExact,
	)
	tracker.snapshot.CountersExact = tracker.snapshot.CountersExact && selection.exact
	tracker.validateExactLocked()
	tracker.publishLocked()
}

func (tracker *receiveProgressTracker) addRecoveredVerified(bytes uint64) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.snapshot.VerifiedBytes, tracker.snapshot.CountersExact = saturatingAdd(
		tracker.snapshot.VerifiedBytes, bytes, tracker.snapshot.CountersExact,
	)
	tracker.validateExactLocked()
	tracker.publishLocked()
}

func (tracker *receiveProgressTracker) addNewlyVerified(bytes uint64) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.snapshot.VerifiedBytes, tracker.snapshot.CountersExact = saturatingAdd(
		tracker.snapshot.VerifiedBytes, bytes, tracker.snapshot.CountersExact,
	)
	tracker.snapshot.NewlyVerifiedBytes, tracker.snapshot.CountersExact = saturatingAdd(
		tracker.snapshot.NewlyVerifiedBytes, bytes, tracker.snapshot.CountersExact,
	)
	tracker.validateExactLocked()
	tracker.publishLocked()
}

func (tracker *receiveProgressTracker) acceptFileSettlement(
	settlement FileSettlement,
	expectedSize uint64,
) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	outcomes := &tracker.snapshot.FileOutcomes
	switch settlement.Kind() {
	case FilePublished:
		if provenance, ok := settlement.PublicationProvenance(); ok {
			switch provenance {
			case FileDownloaded:
				outcomes.DownloadedFiles, tracker.snapshot.CountersExact = saturatingAdd(
					outcomes.DownloadedFiles, 1, tracker.snapshot.CountersExact,
				)
			case FileResumed:
				outcomes.ResumedFiles, tracker.snapshot.CountersExact = saturatingAdd(
					outcomes.ResumedFiles, 1, tracker.snapshot.CountersExact,
				)
			}
		}
		tracker.snapshot.PublishedFiles, tracker.snapshot.CountersExact = saturatingAdd(
			tracker.snapshot.PublishedFiles, 1, tracker.snapshot.CountersExact,
		)
		tracker.snapshot.PublishedBytes, tracker.snapshot.CountersExact = saturatingAdd(
			tracker.snapshot.PublishedBytes, expectedSize, tracker.snapshot.CountersExact,
		)
	case FilePaused:
		outcomes.PausedFiles, tracker.snapshot.CountersExact = saturatingAdd(
			outcomes.PausedFiles, 1, tracker.snapshot.CountersExact,
		)
	case FileCollision:
		outcomes.CollisionFiles, tracker.snapshot.CountersExact = saturatingAdd(
			outcomes.CollisionFiles, 1, tracker.snapshot.CountersExact,
		)
	case FileItemBlocked:
		outcomes.ItemBlockedFiles, tracker.snapshot.CountersExact = saturatingAdd(
			outcomes.ItemBlockedFiles, 1, tracker.snapshot.CountersExact,
		)
		_, reason, hasReason := settlement.ItemBlock()
		if !hasReason {
			break
		}
		switch reason {
		case ItemBlockRevisionConflict:
			outcomes.RevisionConflictFiles, tracker.snapshot.CountersExact = saturatingAdd(
				outcomes.RevisionConflictFiles, 1, tracker.snapshot.CountersExact,
			)
		case ItemBlockCheckpointInvalid:
			outcomes.CheckpointInvalidFiles, tracker.snapshot.CountersExact = saturatingAdd(
				outcomes.CheckpointInvalidFiles, 1, tracker.snapshot.CountersExact,
			)
		case ItemBlockOwnedObjectUnknown:
			outcomes.OwnedObjectUnknownFiles, tracker.snapshot.CountersExact = saturatingAdd(
				outcomes.OwnedObjectUnknownFiles, 1, tracker.snapshot.CountersExact,
			)
		}
	case FileFailed:
		outcomes.FailedFiles, tracker.snapshot.CountersExact = saturatingAdd(
			outcomes.FailedFiles, 1, tracker.snapshot.CountersExact,
		)
	}
	for _, warning := range settlement.MetadataWarnings() {
		if warning == FileMetadataModifiedTime {
			outcomes.ModifiedTimeWarnings, tracker.snapshot.CountersExact = saturatingAdd(
				outcomes.ModifiedTimeWarnings, 1, tracker.snapshot.CountersExact,
			)
		}
	}
	tracker.validateExactLocked()
	tracker.publishLocked()
}

func (tracker *receiveProgressTracker) failDiscovery() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.snapshot.Discovery != DiscoveryOpen {
		return
	}
	tracker.snapshot.Discovery = DiscoveryFailed
	tracker.publishLocked()
}

func (tracker *receiveProgressTracker) finishDiscovery() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.snapshot.Discovery != DiscoveryOpen {
		return
	}
	tracker.snapshot.Discovery = DiscoveryComplete
	tracker.publishLocked()
}

func (tracker *receiveProgressTracker) closeUpdates() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.updates == nil {
		tracker.closed = true
		return
	}
	if tracker.closed {
		return
	}
	tracker.publishLocked()
	close(tracker.updates)
	tracker.closed = true
}

func (tracker *receiveProgressTracker) publishLocked() {
	if tracker.updates == nil || tracker.closed {
		return
	}
	select {
	case <-tracker.updates:
	default:
	}
	tracker.updates <- tracker.snapshot
}

func (tracker *receiveProgressTracker) Updates() <-chan ReceiveProgressSnapshot {
	return tracker.updates
}

func (tracker *receiveProgressTracker) snapshotValue() ReceiveProgressSnapshot {
	tracker.mu.RLock()
	defer tracker.mu.RUnlock()
	return tracker.snapshot
}

func (tracker *receiveProgressTracker) validateExactLocked() {
	if !tracker.snapshot.CountersExact {
		return
	}
	publishedOutcomes, exact := checkedAdd(
		tracker.snapshot.FileOutcomes.DownloadedFiles,
		tracker.snapshot.FileOutcomes.ResumedFiles,
	)
	if !exact || tracker.snapshot.NewlyVerifiedBytes > tracker.snapshot.VerifiedBytes ||
		tracker.snapshot.VerifiedBytes > tracker.snapshot.DiscoveredBytes ||
		tracker.snapshot.PublishedBytes > tracker.snapshot.VerifiedBytes ||
		tracker.snapshot.PublishedFiles > tracker.snapshot.DiscoveredFiles ||
		tracker.snapshot.PublishedFiles != publishedOutcomes {
		// Once an upstream authority violates an exact relationship, consumers must
		// not derive percentages or completion from a misleading exact snapshot.
		tracker.snapshot.CountersExact = false
	}
}

func saturatingAdd(current, delta uint64, exact bool) (uint64, bool) {
	if delta > math.MaxUint64-current {
		return math.MaxUint64, false
	}
	return current + delta, exact
}

func checkedAdd(left, right uint64) (uint64, bool) {
	if right > math.MaxUint64-left {
		return math.MaxUint64, false
	}
	return left + right, true
}

func rangeSetByteCount(ranges content.RangeSet) (uint64, bool) {
	var total uint64
	for _, current := range ranges.Ranges() {
		var exact bool
		total, exact = checkedAdd(total, current.Length())
		if !exact {
			return math.MaxUint64, false
		}
	}
	return total, true
}

// fileTransferProgress retains only the proof necessary to classify one
// unconfirmed write. The operation tracker therefore never becomes a second
// file/range manifest.
type fileTransferProgress struct {
	durable      bool
	trusted      VerifiedDurableRanges
	pending      content.Range
	hasPending   bool
	transientEnd uint64
}

func newFileTransferProgress(
	transaction FileTransaction,
	checkpoint VerifiedDurableRanges,
	durable bool,
) (fileTransferProgress, uint64, bool) {
	if transaction == nil || checkpoint.Binding() != transaction.Binding() ||
		(!durable && !checkpoint.Ranges().IsEmpty()) {
		return fileTransferProgress{}, 0, false
	}
	bytes, exact := rangeSetByteCount(checkpoint.Ranges())
	if !exact || bytes > transaction.Binding().ExactSize() {
		return fileTransferProgress{}, 0, false
	}
	return fileTransferProgress{durable: durable, trusted: checkpoint}, bytes, true
}

func (progress *fileTransferProgress) beginPending(requested content.Range) bool {
	if progress == nil || progress.hasPending || requested.Offset >= requested.End {
		return false
	}
	progress.pending = requested
	progress.hasPending = true
	return true
}

func (progress *fileTransferProgress) acknowledge(
	transaction FileTransaction,
	next VerifiedDurableRanges,
) (uint64, bool) {
	if progress == nil || !progress.hasPending {
		return 0, false
	}
	valid := checkpointExactlyAdvances(transaction, progress.trusted, progress.pending, next)
	if !progress.durable {
		valid = checkpointAcknowledgesTransientWrite(transaction, progress.trusted, next)
	}
	if !valid {
		return 0, false
	}
	return progress.acceptPending(next), true
}

func (progress *fileTransferProgress) reconcileSettlement(
	transaction FileTransaction,
	settlement FileSettlement,
) (uint64, bool) {
	checkpoint, hasCheckpoint := settlement.VerifiedCheckpoint()
	if !hasCheckpoint {
		return 0, true
	}
	if transaction == nil || checkpoint.Binding() != transaction.Binding() {
		return 0, false
	}
	if progress.hasPending {
		if checkpoint.CheckpointGeneration() == progress.trusted.CheckpointGeneration() &&
			exactRangeSetsEqual(checkpoint.Ranges(), progress.trusted.Ranges()) {
			// A terminal settlement may explicitly decline the fully written but
			// unconfirmed interval. Keeping the prior checkpoint is authoritative
			// and must not turn unacknowledged bytes into progress.
			progress.pending = content.Range{}
			progress.hasPending = false
			return 0, true
		}
		return progress.acknowledge(transaction, checkpoint)
	}
	if !exactRangeSetsEqual(checkpoint.Ranges(), progress.trusted.Ranges()) ||
		checkpoint.CheckpointGeneration() < progress.trusted.CheckpointGeneration() {
		return 0, false
	}
	progress.trusted = checkpoint
	return 0, true
}

func (progress *fileTransferProgress) acceptPending(next VerifiedDurableRanges) uint64 {
	delta := progress.pending.Length()
	if !progress.durable {
		progress.transientEnd = progress.pending.End
	}
	progress.trusted = next
	progress.pending = content.Range{}
	progress.hasPending = false
	return delta
}
