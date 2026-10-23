package eventproject

import (
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	appobs "github.com/windshare/windshare/engine/observation"
)

func ProjectProgress(
	value transfer.ReceiveProgressSnapshot,
	capacityWaitVisible bool,
) (appobs.ProgressSnapshot, error) {
	discovery, ok := projectDiscovery(value.Discovery)
	if !ok {
		return appobs.ProgressSnapshot{}, ErrInvalidProjection
	}
	result, err := appobs.NewProgressSnapshot(appobs.ProgressSpec{
		DiscoveredFiles: value.DiscoveredFiles, DiscoveredBytes: value.DiscoveredBytes,
		PublishedFiles: value.PublishedFiles, PublishedBytes: value.PublishedBytes,
		VerifiedBytes: value.VerifiedBytes, NewlyVerifiedBytes: value.NewlyVerifiedBytes,
		PreviouslyPublishedBytes: value.PreviouslyPublishedBytes,
		FileOutcomes:             FileOutcomes(value.FileOutcomes), Discovery: discovery,
		CapacityActiveWaiters:   value.CapacityWait.ActiveWaiters,
		CapacityAccumulatedWait: value.CapacityWait.AccumulatedWait,
		CapacityWaitAttempts:    value.CapacityWait.Attempts,
		CapacityWaitVisible:     capacityWaitVisible,
		CountersExact:           value.CountersExact,
	})
	if err != nil {
		return appobs.ProgressSnapshot{}, ErrInvalidProjection
	}
	return result, nil
}

func ProjectTransferProgress(
	receiveOperation receivecontract.OperationID,
	transferJob transfer.TransferJobID,
	progress transfer.ReceiveProgressSnapshot,
	capacityWaitVisible bool,
) (appobs.TransferProgress, error) {
	receiveID, err := ReceiveOperationID(receiveOperation)
	if err != nil {
		return appobs.TransferProgress{}, ErrInvalidProjection
	}
	jobID, err := TransferJobID(transferJob)
	if err != nil {
		return appobs.TransferProgress{}, ErrInvalidProjection
	}
	snapshot, err := ProjectProgress(progress, capacityWaitVisible)
	if err != nil {
		return appobs.TransferProgress{}, ErrInvalidProjection
	}
	event, err := appobs.NewTransferProgress(receiveID, jobID, snapshot)
	if err != nil {
		return appobs.TransferProgress{}, ErrInvalidProjection
	}
	return event, nil
}

func projectDiscovery(value transfer.DiscoveryStatus) (appobs.DiscoveryStatus, bool) {
	switch value {
	case transfer.DiscoveryOpen:
		return appobs.DiscoveryOpen, true
	case transfer.DiscoveryComplete:
		return appobs.DiscoveryComplete, true
	case transfer.DiscoveryFailed:
		return appobs.DiscoveryFailed, true
	default:
		return 0, false
	}
}

func FileOutcomes(value transfer.FileOutcomeSummary) appobs.FileOutcomes {
	return appobs.FileOutcomes{
		DownloadedFiles: value.DownloadedFiles, ResumedFiles: value.ResumedFiles,
		PreviouslyPublishedFiles: value.PreviouslyPublishedFiles,
		PausedFiles:              value.PausedFiles, CollisionFiles: value.CollisionFiles,
		ItemBlockedFiles: value.ItemBlockedFiles, FailedFiles: value.FailedFiles,
		RevisionConflictFiles:   value.RevisionConflictFiles,
		CheckpointInvalidFiles:  value.CheckpointInvalidFiles,
		OwnedObjectUnknownFiles: value.OwnedObjectUnknownFiles,
		ModifiedTimeWarnings:    value.ModifiedTimeWarnings,
	}
}
