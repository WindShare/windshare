package commandprojection

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func projectSenderTerminalSendTransport(
	value sessionruntime.SenderTerminalSendTransportDisposition,
) (clievent.SenderTerminalSendTransport, bool) {
	switch value {
	case sessionruntime.SenderTerminalSendTransportAccepted:
		return clievent.SenderTerminalSendAccepted, true
	case sessionruntime.SenderTerminalSendTransportNotReached:
		return clievent.SenderTerminalSendNotReached, true
	case sessionruntime.SenderTerminalSendTransportUnsettled:
		return clievent.SenderTerminalSendUnsettled, true
	case sessionruntime.SenderTerminalSendTransportRejected:
		return clievent.SenderTerminalSendRejected, true
	case sessionruntime.SenderTerminalSendTransportRetired:
		return clievent.SenderTerminalSendRetired, true
	default:
		return 0, false
	}
}

func projectSenderTerminalSendOutcome(
	value sessionruntime.SenderTerminalSendOutcome,
) (clievent.SenderTerminalSendOutcome, bool) {
	switch value {
	case sessionruntime.SenderTerminalSendOutcomeDelivered:
		return clievent.SenderTerminalSendDelivered, true
	case sessionruntime.SenderTerminalSendOutcomeDropped:
		return clievent.SenderTerminalSendDropped, true
	case sessionruntime.SenderTerminalSendOutcomeUnknown:
		return clievent.SenderTerminalSendUnknown, true
	default:
		return 0, false
	}
}

func projectSenderTerminalSendDecision(
	value sessionruntime.SenderTerminalSendDecision,
) (clievent.SenderTerminalSendDecision, bool) {
	switch value {
	case sessionruntime.SenderTerminalSendDecisionDelivered:
		return clievent.SenderTerminalSendDecisionDelivered, true
	case sessionruntime.SenderTerminalSendDecisionNaturalRetirement:
		return clievent.SenderTerminalSendNaturalRetirement, true
	case sessionruntime.SenderTerminalSendDecisionFailed:
		return clievent.SenderTerminalSendFailed, true
	default:
		return 0, false
	}
}

func projectSenderSessionTerminalTrigger(
	value sessionruntime.SenderSessionTerminalTrigger,
) (clievent.SenderSessionTerminalTrigger, bool) {
	switch value {
	case sessionruntime.SenderSessionTerminalTriggerGracefulStop:
		return clievent.SenderSessionTerminalGracefulStop, true
	case sessionruntime.SenderSessionTerminalTriggerForcedClose:
		return clievent.SenderSessionTerminalForcedClose, true
	case sessionruntime.SenderSessionTerminalTriggerPeerTerminal:
		return clievent.SenderSessionTerminalPeerTerminal, true
	case sessionruntime.SenderSessionTerminalTriggerPathsExhausted:
		return clievent.SenderSessionTerminalPathsExhausted, true
	case sessionruntime.SenderSessionTerminalTriggerRuntimeFailed:
		return clievent.SenderSessionTerminalRuntimeFailed, true
	default:
		return 0, false
	}
}

func projectSenderSessionTerminalProvenance(
	value sessionruntime.SenderSessionTerminalProvenance,
) (clievent.SenderSessionTerminalProvenance, bool) {
	switch value {
	case sessionruntime.SenderSessionTerminalProvenanceNormalStop:
		return clievent.SenderSessionTerminalNormalStop, true
	case sessionruntime.SenderSessionTerminalProvenanceCallerStop:
		return clievent.SenderSessionTerminalCallerStop, true
	case sessionruntime.SenderSessionTerminalProvenanceRemoteClose:
		return clievent.SenderSessionTerminalRemoteClose, true
	case sessionruntime.SenderSessionTerminalProvenanceLaneRetirement:
		return clievent.SenderSessionTerminalLaneRetirement, true
	case sessionruntime.SenderSessionTerminalProvenanceLocalFault:
		return clievent.SenderSessionTerminalLocalFault, true
	default:
		return 0, false
	}
}

func projectCatalogStorageOperation(value liveshare.CatalogStorageOperation) (clievent.CatalogStorageOperation, bool) {
	switch value {
	case liveshare.CatalogStorageCreating:
		return clievent.CatalogStorageCreating, true
	case liveshare.CatalogStorageCreated:
		return clievent.CatalogStorageCreated, true
	case liveshare.CatalogStorageRecovering:
		return clievent.CatalogStorageRecovering, true
	case liveshare.CatalogStorageRecovered:
		return clievent.CatalogStorageRecovered, true
	case liveshare.CatalogStorageBudgetRejected:
		return clievent.CatalogStorageBudgetRejected, true
	case liveshare.CatalogStorageCleaning:
		return clievent.CatalogStorageCleaning, true
	case liveshare.CatalogStorageCleaned:
		return clievent.CatalogStorageCleaned, true
	default:
		return 0, false
	}
}

func projectCatalogStorageCause(value liveshare.CatalogStorageCause) (clievent.CatalogStorageCause, bool) {
	switch value {
	case liveshare.CatalogStorageCauseNone:
		return clievent.CatalogStorageCauseNone, true
	case liveshare.CatalogStorageCauseCanceled:
		return clievent.CatalogStorageCauseCanceled, true
	case liveshare.CatalogStorageCauseDeadlineExceeded:
		return clievent.CatalogStorageCauseDeadline, true
	case liveshare.CatalogStorageCauseBudgetExceeded:
		return clievent.CatalogStorageCauseBudget, true
	case liveshare.CatalogStorageCauseUnexpected:
		return clievent.CatalogStorageCauseUnexpected, true
	default:
		return 0, false
	}
}

func projectRootPrefetchDecision(value liveshare.RootPrefetchDecision) (clievent.RootPrefetchDecision, bool) {
	switch value {
	case liveshare.RootPrefetchAttemptStarted:
		return clievent.RootPrefetchAttemptStarted, true
	case liveshare.RootPrefetchYieldedToDemand:
		return clievent.RootPrefetchYieldedToDemand, true
	case liveshare.RootPrefetchRetryScheduled:
		return clievent.RootPrefetchRetryScheduled, true
	case liveshare.RootPrefetchCommitted:
		return clievent.RootPrefetchCommitted, true
	case liveshare.RootPrefetchBudgetFailed:
		return clievent.RootPrefetchBudgetFailed, true
	case liveshare.RootPrefetchScanFailed:
		return clievent.RootPrefetchScanFailed, true
	case liveshare.RootPrefetchStopped:
		return clievent.RootPrefetchStopped, true
	default:
		return 0, false
	}
}
