package eventproject

import (
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/sessionruntime"
	appobs "github.com/windshare/windshare/engine/observation"
)

func projectSenderTerminalSendTransport(
	value sessionruntime.SenderTerminalSendTransportDisposition,
) (appobs.SenderTerminalSendTransport, bool) {
	switch value {
	case sessionruntime.SenderTerminalSendTransportAccepted:
		return appobs.SenderTerminalSendAccepted, true
	case sessionruntime.SenderTerminalSendTransportNotReached:
		return appobs.SenderTerminalSendNotReached, true
	case sessionruntime.SenderTerminalSendTransportUnsettled:
		return appobs.SenderTerminalSendUnsettled, true
	case sessionruntime.SenderTerminalSendTransportRejected:
		return appobs.SenderTerminalSendRejected, true
	case sessionruntime.SenderTerminalSendTransportRetired:
		return appobs.SenderTerminalSendRetired, true
	default:
		return 0, false
	}
}

func projectSenderTerminalSendOutcome(
	value sessionruntime.SenderTerminalSendOutcome,
) (appobs.SenderTerminalSendOutcome, bool) {
	switch value {
	case sessionruntime.SenderTerminalSendOutcomeDelivered:
		return appobs.SenderTerminalSendDelivered, true
	case sessionruntime.SenderTerminalSendOutcomeDropped:
		return appobs.SenderTerminalSendDropped, true
	case sessionruntime.SenderTerminalSendOutcomeUnknown:
		return appobs.SenderTerminalSendUnknown, true
	default:
		return 0, false
	}
}

func projectSenderTerminalSendDecision(
	value sessionruntime.SenderTerminalSendDecision,
) (appobs.SenderTerminalSendDecision, bool) {
	switch value {
	case sessionruntime.SenderTerminalSendDecisionDelivered:
		return appobs.SenderTerminalSendDecisionDelivered, true
	case sessionruntime.SenderTerminalSendDecisionNaturalRetirement:
		return appobs.SenderTerminalSendNaturalRetirement, true
	case sessionruntime.SenderTerminalSendDecisionFailed:
		return appobs.SenderTerminalSendFailed, true
	default:
		return 0, false
	}
}

func projectSenderSessionTerminalTrigger(
	value sessionruntime.SenderSessionTerminalTrigger,
) (appobs.SenderSessionTerminalTrigger, bool) {
	switch value {
	case sessionruntime.SenderSessionTerminalTriggerGracefulStop:
		return appobs.SenderSessionTerminalGracefulStop, true
	case sessionruntime.SenderSessionTerminalTriggerForcedClose:
		return appobs.SenderSessionTerminalForcedClose, true
	case sessionruntime.SenderSessionTerminalTriggerPeerTerminal:
		return appobs.SenderSessionTerminalPeerTerminal, true
	case sessionruntime.SenderSessionTerminalTriggerPathsExhausted:
		return appobs.SenderSessionTerminalPathsExhausted, true
	case sessionruntime.SenderSessionTerminalTriggerRuntimeFailed:
		return appobs.SenderSessionTerminalRuntimeFailed, true
	default:
		return 0, false
	}
}

func projectSenderSessionTerminalProvenance(
	value sessionruntime.SenderSessionTerminalProvenance,
) (appobs.SenderSessionTerminalProvenance, bool) {
	switch value {
	case sessionruntime.SenderSessionTerminalProvenanceNormalStop:
		return appobs.SenderSessionTerminalNormalStop, true
	case sessionruntime.SenderSessionTerminalProvenanceCallerStop:
		return appobs.SenderSessionTerminalCallerStop, true
	case sessionruntime.SenderSessionTerminalProvenanceRemoteClose:
		return appobs.SenderSessionTerminalRemoteClose, true
	case sessionruntime.SenderSessionTerminalProvenanceLaneRetirement:
		return appobs.SenderSessionTerminalLaneRetirement, true
	case sessionruntime.SenderSessionTerminalProvenanceLocalFault:
		return appobs.SenderSessionTerminalLocalFault, true
	default:
		return 0, false
	}
}

func projectCatalogStorageOperation(value liveshare.CatalogStorageOperation) (appobs.CatalogStorageOperation, bool) {
	switch value {
	case liveshare.CatalogStorageCreating:
		return appobs.CatalogStorageCreating, true
	case liveshare.CatalogStorageCreated:
		return appobs.CatalogStorageCreated, true
	case liveshare.CatalogStorageRecovering:
		return appobs.CatalogStorageRecovering, true
	case liveshare.CatalogStorageRecovered:
		return appobs.CatalogStorageRecovered, true
	case liveshare.CatalogStorageBudgetRejected:
		return appobs.CatalogStorageBudgetRejected, true
	case liveshare.CatalogStorageCleaning:
		return appobs.CatalogStorageCleaning, true
	case liveshare.CatalogStorageCleaned:
		return appobs.CatalogStorageCleaned, true
	default:
		return 0, false
	}
}

func projectCatalogStorageCause(value liveshare.CatalogStorageCause) (appobs.CatalogStorageCause, bool) {
	switch value {
	case liveshare.CatalogStorageCauseNone:
		return appobs.CatalogStorageCauseNone, true
	case liveshare.CatalogStorageCauseCanceled:
		return appobs.CatalogStorageCauseCanceled, true
	case liveshare.CatalogStorageCauseDeadlineExceeded:
		return appobs.CatalogStorageCauseDeadline, true
	case liveshare.CatalogStorageCauseBudgetExceeded:
		return appobs.CatalogStorageCauseBudget, true
	case liveshare.CatalogStorageCauseUnexpected:
		return appobs.CatalogStorageCauseUnexpected, true
	default:
		return 0, false
	}
}

func projectRootPrefetchDecision(value liveshare.RootPrefetchDecision) (appobs.RootPrefetchDecision, bool) {
	switch value {
	case liveshare.RootPrefetchAttemptStarted:
		return appobs.RootPrefetchAttemptStarted, true
	case liveshare.RootPrefetchYieldedToDemand:
		return appobs.RootPrefetchYieldedToDemand, true
	case liveshare.RootPrefetchRetryScheduled:
		return appobs.RootPrefetchRetryScheduled, true
	case liveshare.RootPrefetchCommitted:
		return appobs.RootPrefetchCommitted, true
	case liveshare.RootPrefetchBudgetFailed:
		return appobs.RootPrefetchBudgetFailed, true
	case liveshare.RootPrefetchScanFailed:
		return appobs.RootPrefetchScanFailed, true
	case liveshare.RootPrefetchStopped:
		return appobs.RootPrefetchStopped, true
	default:
		return 0, false
	}
}
