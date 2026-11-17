package observation

type SafeMessageKey uint8

const (
	MessageUnexpected SafeMessageKey = iota + 1
	MessageInterrupted
	MessageTimedOut
	MessageInvalidRequest
	MessageNetworkUnavailable
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
	MessageSourceOmitted
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
	MessageSelectionCollision
	MessageShareStopped
	MessageOutputBusy

	// messageKeyLimit must stay last; it is not a valid key.
	messageKeyLimit
)

var safeMessageKeyNames = [...]string{
	MessageUnexpected:                 "unexpected",
	MessageInterrupted:                "interrupted",
	MessageTimedOut:                   "timed_out",
	MessageInvalidRequest:             "invalid_request",
	MessageNetworkUnavailable:         "network_unavailable",
	MessageCapabilityInvalid:          "capability_invalid",
	MessageSelectionMissing:           "selection_missing",
	MessageRelayRejected:              "relay_rejected",
	MessageRelayUnavailable:           "relay_unavailable",
	MessageShareUnavailable:           "share_unavailable",
	MessageDirectUnavailable:          "direct_unavailable",
	MessageDirectPolicy:               "direct_policy",
	MessageDirectBusy:                 "direct_busy",
	MessageDirectAdmission:            "direct_admission",
	MessageSourceUnavailable:          "source_unavailable",
	MessageSourceMissing:              "source_missing",
	MessageSourceAccessDenied:         "source_access_denied",
	MessageSourceUnsupported:          "source_unsupported",
	MessageSourceOmitted:              "source_omitted",
	MessageSourceChanged:              "source_changed",
	MessageCatalogUnavailable:         "catalog_unavailable",
	MessageSessionFailed:              "session_failed",
	MessageOutputFailed:               "output_failed",
	MessageCheckpointFailed:           "checkpoint_failed",
	MessagePublicationFailed:          "publication_failed",
	MessageTraceIncomplete:            "trace_incomplete",
	MessageTraceExists:                "trace_exists",
	MessageOutputNeedsAttention:       "output_needs_attention",
	MessageDestinationCollision:       "destination_collision",
	MessageCheckpointRevisionConflict: "checkpoint_revision_conflict",
	MessageCheckpointInvalid:          "checkpoint_invalid",
	MessageOwnedObjectUnknown:         "owned_object_unknown",
	MessageOutputRecoveryUnavailable:  "output_recovery_unavailable",
	MessageRetainedOutputMissing:      "retained_output_missing",
	MessageSelectionCollision:         "selection_collision",
	MessageShareStopped:               "share_stopped",
	MessageOutputBusy:                 "output_busy",
}

// Every declared key must have a table entry: this fails to compile when the
// table and the constant list differ in length.
var _ [0]struct{} = [len(safeMessageKeyNames) - int(messageKeyLimit)]struct{}{}

func (value SafeMessageKey) Name() (string, bool) {
	if value < MessageUnexpected || int(value) >= len(safeMessageKeyNames) {
		return "", false
	}
	return safeMessageKeyNames[value], true
}
