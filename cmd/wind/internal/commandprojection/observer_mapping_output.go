package commandprojection

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/transfer"
)

func projectTransferStage(value transfer.TransferLifecycleStage) (clievent.TransferLifecycleStage, bool) {
	switch value {
	case transfer.TransferDiscoveryStarted:
		return clievent.TransferDiscoveryStarted, true
	case transfer.TransferGenerationCommitted:
		return clievent.TransferGenerationCommitted, true
	case transfer.TransferDiscoveryCompleted:
		return clievent.TransferDiscoveryCompleted, true
	case transfer.TransferAdmissionStarted:
		return clievent.TransferAdmissionStarted, true
	case transfer.TransferAdmissionCompleted:
		return clievent.TransferAdmissionCompleted, true
	case transfer.TransferDirectoryAdmitted:
		return clievent.TransferDirectoryAdmitted, true
	case transfer.TransferDirectoryFinalized:
		return clievent.TransferDirectoryFinalized, true
	case transfer.TransferFileEnqueued:
		return clievent.TransferFileEnqueued, true
	case transfer.TransferFileStarted:
		return clievent.TransferFileStarted, true
	case transfer.TransferFileAdmitted:
		return clievent.TransferFileAdmitted, true
	case transfer.TransferFileFirstWrite:
		return clievent.TransferFileFirstWrite, true
	case transfer.TransferFileSettled:
		return clievent.TransferFileSettled, true
	case transfer.TransferJobSettled:
		return clievent.TransferJobSettled, true
	default:
		return 0, false
	}
}

func projectFileSelection(value transfer.FileSelectionDecision) (clievent.FileSelectionDecision, bool) {
	switch value {
	case 0:
		return clievent.FileSelectionNone, true
	case transfer.FileSelectionInherited:
		return clievent.FileSelectionInherited, true
	case transfer.FileSelectionNodeOverride:
		return clievent.FileSelectionNodeOverride, true
	case transfer.FileSelectionCatalogPathTarget:
		return clievent.FileSelectionCatalogPathTarget, true
	default:
		return 0, false
	}
}

func projectFileSettlement(value transfer.FileSettlementKind) (clievent.FileSettlement, bool) {
	switch value {
	case 0:
		return clievent.FileSettlementNone, true
	case transfer.FilePublished:
		return clievent.FilePublished, true
	case transfer.FilePaused:
		return clievent.FilePaused, true
	case transfer.FileCollision:
		return clievent.FileCollision, true
	case transfer.FileItemBlocked:
		return clievent.FileItemBlocked, true
	case transfer.FileFailed:
		return clievent.FileFailed, true
	default:
		return 0, false
	}
}

func projectItemBlockReason(value transfer.ItemBlockReason) (clievent.ItemBlockReason, bool) {
	switch value {
	case 0:
		return clievent.ItemBlockNone, true
	case transfer.ItemBlockStateCorrupt:
		return clievent.ItemBlockStateCorrupt, true
	case transfer.ItemBlockOwnershipUnknown:
		return clievent.ItemBlockOwnershipUnknown, true
	case transfer.ItemBlockPublicationAmbiguous:
		return clievent.ItemBlockPublicationAmbiguous, true
	case transfer.ItemBlockRetirementUncertain:
		return clievent.ItemBlockRetirementUncertain, true
	case transfer.ItemBlockRevisionConflict:
		return clievent.ItemBlockRevisionConflict, true
	case transfer.ItemBlockCheckpointInvalid:
		return clievent.ItemBlockCheckpointInvalid, true
	case transfer.ItemBlockOwnedObjectUnknown:
		return clievent.ItemBlockOwnedObjectUnknown, true
	default:
		return 0, false
	}
}

func projectTreeSettlement(value transfer.DirectTreeSettlementKind) (clievent.TreeSettlement, bool) {
	switch value {
	case 0:
		return clievent.TreeSettlementNone, true
	case transfer.DirectTreeSettlementSuccess:
		return clievent.TreeSettlementSuccess, true
	case transfer.DirectTreeSettlementPartial:
		return clievent.TreeSettlementPartial, true
	case transfer.DirectTreeSettlementPaused:
		return clievent.TreeSettlementPaused, true
	case transfer.DirectTreeSettlementFailed:
		return clievent.TreeSettlementFailed, true
	default:
		return 0, false
	}
}
