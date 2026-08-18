package commandprojection

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func ProjectProgress(value transfer.ReceiveProgressSnapshot) (clievent.ProgressSnapshot, error) {
	discovery, ok := projectDiscovery(value.Discovery)
	if !ok {
		return clievent.ProgressSnapshot{}, ErrInvalidProjection
	}
	result, err := clievent.NewProgressSnapshot(clievent.ProgressSpec{
		DiscoveredFiles: value.DiscoveredFiles, DiscoveredBytes: value.DiscoveredBytes,
		PublishedFiles: value.PublishedFiles, PublishedBytes: value.PublishedBytes,
		VerifiedBytes: value.VerifiedBytes, NewlyVerifiedBytes: value.NewlyVerifiedBytes,
		FileOutcomes: projectFileOutcomes(value.FileOutcomes), Discovery: discovery,
		CountersExact: value.CountersExact,
	})
	if err != nil {
		return clievent.ProgressSnapshot{}, ErrInvalidProjection
	}
	return result, nil
}

func ProjectTransferProgress(
	receiveOperation receivecontract.OperationID,
	transferJob transfer.TransferJobID,
	progress transfer.ReceiveProgressSnapshot,
) (clievent.TransferProgress, error) {
	receiveID, err := ReceiveOperationID(receiveOperation)
	if err != nil {
		return clievent.TransferProgress{}, ErrInvalidProjection
	}
	jobID, err := TransferJobID(transferJob)
	if err != nil {
		return clievent.TransferProgress{}, ErrInvalidProjection
	}
	snapshot, err := ProjectProgress(progress)
	if err != nil {
		return clievent.TransferProgress{}, ErrInvalidProjection
	}
	event, err := clievent.NewTransferProgress(receiveID, jobID, snapshot)
	if err != nil {
		return clievent.TransferProgress{}, ErrInvalidProjection
	}
	return event, nil
}

func projectDiscovery(value transfer.DiscoveryStatus) (clievent.DiscoveryStatus, bool) {
	switch value {
	case transfer.DiscoveryOpen:
		return clievent.DiscoveryOpen, true
	case transfer.DiscoveryComplete:
		return clievent.DiscoveryComplete, true
	case transfer.DiscoveryFailed:
		return clievent.DiscoveryFailed, true
	default:
		return 0, false
	}
}

func projectFileOutcomes(value transfer.FileOutcomeSummary) clievent.FileOutcomes {
	return clievent.FileOutcomes{
		DownloadedFiles: value.DownloadedFiles, ResumedFiles: value.ResumedFiles,
		PausedFiles: value.PausedFiles, CollisionFiles: value.CollisionFiles,
		ItemBlockedFiles: value.ItemBlockedFiles, FailedFiles: value.FailedFiles,
		ModifiedTimeWarnings: value.ModifiedTimeWarnings,
	}
}
