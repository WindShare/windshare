package clievent

import (
	"errors"
	"time"
)

var ErrInvalidProgress = errors.New("CLI receive progress is invalid")

type FileOutcomes struct {
	DownloadedFiles         uint64
	ResumedFiles            uint64
	PausedFiles             uint64
	CollisionFiles          uint64
	ItemBlockedFiles        uint64
	RevisionConflictFiles   uint64
	CheckpointInvalidFiles  uint64
	OwnedObjectUnknownFiles uint64
	FailedFiles             uint64
	ModifiedTimeWarnings    uint64
}

func (outcomes FileOutcomes) HasNonSuccess() bool {
	return outcomes.PausedFiles != 0 || outcomes.CollisionFiles != 0 ||
		outcomes.ItemBlockedFiles != 0 || outcomes.FailedFiles != 0
}

type ProgressSpec struct {
	DiscoveredFiles    uint64
	DiscoveredBytes    uint64
	PublishedFiles     uint64
	PublishedBytes     uint64
	VerifiedBytes      uint64
	NewlyVerifiedBytes uint64
	FileOutcomes       FileOutcomes
	// Capacity pressure is receiver scheduling state, not file settlement, so it
	// must remain outside the exact counter relationships above.
	CapacityActiveWaiters   uint32
	CapacityAccumulatedWait time.Duration
	CapacityWaitAttempts    uint64
	CapacityWaitVisible     bool
	Discovery               DiscoveryStatus
	CountersExact           bool
}

type ProgressSnapshot struct {
	discoveredFiles         uint64
	discoveredBytes         uint64
	publishedFiles          uint64
	publishedBytes          uint64
	verifiedBytes           uint64
	newlyVerifiedBytes      uint64
	fileOutcomes            FileOutcomes
	capacityActiveWaiters   uint32
	capacityAccumulatedWait time.Duration
	capacityWaitAttempts    uint64
	capacityWaitVisible     bool
	discovery               DiscoveryStatus
	countersExact           bool
}

func NewProgressSnapshot(spec ProgressSpec) (ProgressSnapshot, error) {
	if !spec.Discovery.Valid() || spec.CapacityAccumulatedWait < 0 ||
		(spec.CapacityWaitVisible && spec.CapacityActiveWaiters == 0) {
		return ProgressSnapshot{}, ErrInvalidProgress
	}
	if spec.CountersExact {
		publishedOutcomes, overflow := checkedAdd(
			spec.FileOutcomes.DownloadedFiles,
			spec.FileOutcomes.ResumedFiles,
		)
		if overflow || !validExactItemBlockCounts(spec.FileOutcomes) ||
			spec.NewlyVerifiedBytes > spec.VerifiedBytes ||
			spec.VerifiedBytes > spec.DiscoveredBytes ||
			spec.PublishedBytes > spec.VerifiedBytes ||
			spec.PublishedFiles > spec.DiscoveredFiles ||
			spec.PublishedFiles != publishedOutcomes {
			return ProgressSnapshot{}, ErrInvalidProgress
		}
	}
	return ProgressSnapshot{
		discoveredFiles: spec.DiscoveredFiles, discoveredBytes: spec.DiscoveredBytes,
		publishedFiles: spec.PublishedFiles, publishedBytes: spec.PublishedBytes,
		verifiedBytes: spec.VerifiedBytes, newlyVerifiedBytes: spec.NewlyVerifiedBytes,
		fileOutcomes:            spec.FileOutcomes,
		capacityActiveWaiters:   spec.CapacityActiveWaiters,
		capacityAccumulatedWait: spec.CapacityAccumulatedWait,
		capacityWaitAttempts:    spec.CapacityWaitAttempts,
		capacityWaitVisible:     spec.CapacityWaitVisible,
		discovery:               spec.Discovery,
		countersExact:           spec.CountersExact,
	}, nil
}

func validExactItemBlockCounts(outcomes FileOutcomes) bool {
	classified, overflow := checkedAdd(outcomes.RevisionConflictFiles, outcomes.CheckpointInvalidFiles)
	classified, ownershipOverflow := checkedAdd(classified, outcomes.OwnedObjectUnknownFiles)
	return !overflow && !ownershipOverflow && classified <= outcomes.ItemBlockedFiles
}

func checkedAdd(left, right uint64) (uint64, bool) {
	sum := left + right
	return sum, sum < left
}

func (snapshot ProgressSnapshot) DiscoveredFiles() uint64    { return snapshot.discoveredFiles }
func (snapshot ProgressSnapshot) DiscoveredBytes() uint64    { return snapshot.discoveredBytes }
func (snapshot ProgressSnapshot) PublishedFiles() uint64     { return snapshot.publishedFiles }
func (snapshot ProgressSnapshot) PublishedBytes() uint64     { return snapshot.publishedBytes }
func (snapshot ProgressSnapshot) VerifiedBytes() uint64      { return snapshot.verifiedBytes }
func (snapshot ProgressSnapshot) NewlyVerifiedBytes() uint64 { return snapshot.newlyVerifiedBytes }
func (snapshot ProgressSnapshot) FileOutcomes() FileOutcomes { return snapshot.fileOutcomes }
func (snapshot ProgressSnapshot) CapacityActiveWaiters() uint32 {
	return snapshot.capacityActiveWaiters
}
func (snapshot ProgressSnapshot) CapacityAccumulatedWait() time.Duration {
	return snapshot.capacityAccumulatedWait
}
func (snapshot ProgressSnapshot) CapacityWaitAttempts() uint64 {
	return snapshot.capacityWaitAttempts
}
func (snapshot ProgressSnapshot) CapacityWaitVisible() bool  { return snapshot.capacityWaitVisible }
func (snapshot ProgressSnapshot) Discovery() DiscoveryStatus { return snapshot.discovery }
func (snapshot ProgressSnapshot) CountersExact() bool        { return snapshot.countersExact }
func (snapshot ProgressSnapshot) Valid() bool {
	_, err := NewProgressSnapshot(ProgressSpec{
		DiscoveredFiles: snapshot.discoveredFiles, DiscoveredBytes: snapshot.discoveredBytes,
		PublishedFiles: snapshot.publishedFiles, PublishedBytes: snapshot.publishedBytes,
		VerifiedBytes: snapshot.verifiedBytes, NewlyVerifiedBytes: snapshot.newlyVerifiedBytes,
		FileOutcomes:            snapshot.fileOutcomes,
		CapacityActiveWaiters:   snapshot.capacityActiveWaiters,
		CapacityAccumulatedWait: snapshot.capacityAccumulatedWait,
		CapacityWaitAttempts:    snapshot.capacityWaitAttempts,
		CapacityWaitVisible:     snapshot.capacityWaitVisible,
		Discovery:               snapshot.discovery,
		CountersExact:           snapshot.countersExact,
	})
	return err == nil
}
