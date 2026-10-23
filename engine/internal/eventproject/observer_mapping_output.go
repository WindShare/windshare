package eventproject

import (
	"github.com/windshare/windshare/core/transfer"
	appobs "github.com/windshare/windshare/engine/observation"
)

func projectTransferStage(value transfer.TransferLifecycleStage) (appobs.TransferLifecycleStage, bool) {
	switch value {
	case transfer.TransferDiscoveryStarted:
		return appobs.TransferDiscoveryStarted, true
	case transfer.TransferGenerationCommitted:
		return appobs.TransferGenerationCommitted, true
	case transfer.TransferDiscoveryCompleted:
		return appobs.TransferDiscoveryCompleted, true
	case transfer.TransferAdmissionStarted:
		return appobs.TransferAdmissionStarted, true
	case transfer.TransferAdmissionCompleted:
		return appobs.TransferAdmissionCompleted, true
	case transfer.TransferDirectoryAdmitted:
		return appobs.TransferDirectoryAdmitted, true
	case transfer.TransferDirectoryFinalized:
		return appobs.TransferDirectoryFinalized, true
	case transfer.TransferFileEnqueued:
		return appobs.TransferFileEnqueued, true
	case transfer.TransferFileStarted:
		return appobs.TransferFileStarted, true
	case transfer.TransferFileAdmitted:
		return appobs.TransferFileAdmitted, true
	case transfer.TransferFileFirstWrite:
		return appobs.TransferFileFirstWrite, true
	case transfer.TransferFileSettled:
		return appobs.TransferFileSettled, true
	case transfer.TransferCapacityRetryScheduled:
		return appobs.TransferCapacityRetryScheduled, true
	case transfer.TransferCapacityRetryReady:
		return appobs.TransferCapacityRetryReady, true
	case transfer.TransferCapacityRetrySucceeded:
		return appobs.TransferCapacityRetrySucceeded, true
	case transfer.TransferCapacityBudgetPaused:
		return appobs.TransferCapacityBudgetPaused, true
	case transfer.TransferCapacityWaitCanceled:
		return appobs.TransferCapacityWaitCanceled, true
	case transfer.TransferCapacityGenerationEnded:
		return appobs.TransferCapacityGenerationEnded, true
	case transfer.TransferJobSettled:
		return appobs.TransferJobSettled, true
	default:
		return 0, false
	}
}

func projectFileSelection(value transfer.FileSelectionDecision) (appobs.FileSelectionDecision, bool) {
	switch value {
	case 0:
		return appobs.FileSelectionNone, true
	case transfer.FileSelectionInherited:
		return appobs.FileSelectionInherited, true
	case transfer.FileSelectionNodeOverride:
		return appobs.FileSelectionNodeOverride, true
	case transfer.FileSelectionCatalogPathTarget:
		return appobs.FileSelectionCatalogPathTarget, true
	default:
		return 0, false
	}
}

func projectFileSettlement(value transfer.FileSettlementKind) (appobs.FileSettlement, bool) {
	switch value {
	case 0:
		return appobs.FileSettlementNone, true
	case transfer.FilePreviouslyPublished:
		return appobs.FilePreviouslyPublished, true
	case transfer.FilePublished:
		return appobs.FilePublished, true
	case transfer.FilePaused:
		return appobs.FilePaused, true
	case transfer.FileCollision:
		return appobs.FileCollision, true
	case transfer.FileItemBlocked:
		return appobs.FileItemBlocked, true
	case transfer.FileFailed:
		return appobs.FileFailed, true
	default:
		return 0, false
	}
}

func projectItemBlockReason(value transfer.ItemBlockReason) (appobs.ItemBlockReason, bool) {
	switch value {
	case 0:
		return appobs.ItemBlockNone, true
	case transfer.ItemBlockStateCorrupt:
		return appobs.ItemBlockStateCorrupt, true
	case transfer.ItemBlockOwnershipUnknown:
		return appobs.ItemBlockOwnershipUnknown, true
	case transfer.ItemBlockPublicationAmbiguous:
		return appobs.ItemBlockPublicationAmbiguous, true
	case transfer.ItemBlockRetirementUncertain:
		return appobs.ItemBlockRetirementUncertain, true
	case transfer.ItemBlockRevisionConflict:
		return appobs.ItemBlockRevisionConflict, true
	case transfer.ItemBlockCheckpointInvalid:
		return appobs.ItemBlockCheckpointInvalid, true
	case transfer.ItemBlockOwnedObjectUnknown:
		return appobs.ItemBlockOwnedObjectUnknown, true
	default:
		return 0, false
	}
}

func projectTreeSettlement(value transfer.DirectTreeSettlementKind) (appobs.TreeSettlement, bool) {
	switch value {
	case 0:
		return appobs.TreeSettlementNone, true
	case transfer.DirectTreeSettlementSuccess:
		return appobs.TreeSettlementSuccess, true
	case transfer.DirectTreeSettlementPartial:
		return appobs.TreeSettlementPartial, true
	case transfer.DirectTreeSettlementPaused:
		return appobs.TreeSettlementPaused, true
	case transfer.DirectTreeSettlementFailed:
		return appobs.TreeSettlementFailed, true
	default:
		return 0, false
	}
}
