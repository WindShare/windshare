package observation

type SafeMessageKey uint8

const (
	MessageUnexpected SafeMessageKey = iota + 1
	MessageInterrupted
	MessageTimedOut
	MessageInvalidRequest
	MessageCapabilityInvalid
	MessageSelectionMissing
	MessageRelayRejected
	MessageRelayUnavailable
	MessageShareUnavailable
	MessageDirectUnavailable
	MessageDirectPolicy
	MessageDirectBusy
	MessageDirectAdmission
	MessageSourceUnavailable
	MessageSourceMissing
	MessageSourceAccessDenied
	MessageSourceUnsupported
	MessageSourceChanged
	MessageCatalogUnavailable
	MessageSessionFailed
	MessageOutputFailed
	MessageCheckpointFailed
	MessagePublicationFailed
	MessageTraceIncomplete
	MessageTraceExists
	MessageOutputNeedsAttention
	MessageDestinationCollision
	MessageCheckpointRevisionConflict
	MessageCheckpointInvalid
	MessageOwnedObjectUnknown
	MessageOutputRecoveryUnavailable
	MessageRetainedOutputMissing
)

func (value SafeMessageKey) Name() (string, bool) {
	switch value {
	case MessageUnexpected:
		return "unexpected", true
	case MessageInterrupted:
		return "interrupted", true
	case MessageTimedOut:
		return "timed_out", true
	case MessageInvalidRequest:
		return "invalid_request", true
	case MessageCapabilityInvalid:
		return "capability_invalid", true
	case MessageSelectionMissing:
		return "selection_missing", true
	case MessageRelayRejected:
		return "relay_rejected", true
	case MessageRelayUnavailable:
		return "relay_unavailable", true
	case MessageShareUnavailable:
		return "share_unavailable", true
	case MessageDirectUnavailable:
		return "direct_unavailable", true
	case MessageDirectPolicy:
		return "direct_policy", true
	case MessageDirectBusy:
		return "direct_busy", true
	case MessageDirectAdmission:
		return "direct_admission", true
	case MessageSourceUnavailable:
		return "source_unavailable", true
	case MessageSourceMissing:
		return "source_missing", true
	case MessageSourceAccessDenied:
		return "source_access_denied", true
	case MessageSourceUnsupported:
		return "source_unsupported", true
	case MessageSourceChanged:
		return "source_changed", true
	case MessageCatalogUnavailable:
		return "catalog_unavailable", true
	case MessageSessionFailed:
		return "session_failed", true
	case MessageOutputFailed:
		return "output_failed", true
	case MessageCheckpointFailed:
		return "checkpoint_failed", true
	case MessagePublicationFailed:
		return "publication_failed", true
	case MessageTraceIncomplete:
		return "trace_incomplete", true
	case MessageTraceExists:
		return "trace_exists", true
	case MessageOutputNeedsAttention:
		return "output_needs_attention", true
	case MessageDestinationCollision:
		return "destination_collision", true
	case MessageCheckpointRevisionConflict:
		return "checkpoint_revision_conflict", true
	case MessageCheckpointInvalid:
		return "checkpoint_invalid", true
	case MessageOwnedObjectUnknown:
		return "owned_object_unknown", true
	case MessageOutputRecoveryUnavailable:
		return "output_recovery_unavailable", true
	case MessageRetainedOutputMissing:
		return "retained_output_missing", true
	default:
		return "", false
	}
}
