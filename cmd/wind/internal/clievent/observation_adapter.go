package clievent

import (
	fsobs "github.com/windshare/windshare/adapters/filesystem/diagnostic"
	"github.com/windshare/windshare/core/diagnosticerror"
	appobs "github.com/windshare/windshare/engine/observation"
)

func NewLaneSettlementObserved(spec appobs.LaneSettlementSpec) (LaneSettlementObserved, error) {
	value, err := appobs.NewLaneSettlementObserved(spec)
	return LaneSettlementObserved{value}, err
}

func NewObserverLossObserved(spec appobs.ObserverLossSpec) (ObserverLossObserved, error) {
	value, err := appobs.NewObserverLossObserved(spec)
	return ObserverLossObserved{value}, err
}

func NewReceiverTerminationObserved(spec appobs.ReceiverTerminationSpec) (ReceiverTerminationObserved, error) {
	value, err := appobs.NewReceiverTerminationObserved(spec)
	return ReceiverTerminationObserved{value}, err
}

func NewEngineTaskObserved(spec appobs.EngineTaskSpec) (EngineTaskObserved, error) {
	value, err := appobs.NewEngineTaskObserved(spec)
	return EngineTaskObserved{value}, err
}

func NewReady() Ready { return Ready{appobs.NewReady()} }

func NewSharingSubjectSelected(subject appobs.SharingSubject) (SharingSubjectSelected, error) {
	value, err := appobs.NewSharingSubjectSelected(subject)
	return SharingSubjectSelected{value}, err
}

func NewRelayConnected(command Command, authority appobs.RelayAuthority) (RelayConnected, error) {
	value, err := appobs.NewRelayConnected(WorkflowForCommand(command), authority)
	return RelayConnected{value}, err
}

func NewRelayRecovering(
	command Command,
	authority appobs.RelayAuthority,
	attempt uint32,
	state appobs.RelayRecoveryState,
	failure appobs.Failure,
) (RelayRecovering, error) {
	value, err := appobs.NewRelayRecovering(WorkflowForCommand(command), authority, attempt, state, failure)
	return RelayRecovering{value}, err
}

func NewContentPathSelected(path appobs.ContentPath) (ContentPathSelected, error) {
	value, err := appobs.NewContentPathSelected(path)
	return ContentPathSelected{value}, err
}

func NewFallback(command Command, from, to appobs.Transport, failure appobs.Failure) (Fallback, error) {
	value, err := appobs.NewFallback(WorkflowForCommand(command), from, to, failure)
	return Fallback{value}, err
}

func NewTransferProgress(
	receiveOperation appobs.ReceiveOperationID,
	transferJob appobs.TransferJobID,
	snapshot appobs.ProgressSnapshot,
) (TransferProgress, error) {
	value, err := appobs.NewTransferProgress(receiveOperation, transferJob, snapshot)
	return TransferProgress{value}, err
}

func NewWarning(command Command, failure appobs.Failure) (Warning, error) {
	value, err := appobs.NewWarning(WorkflowForCommand(command), failure)
	return Warning{value}, err
}

func NewLaneAdopted(
	command Command,
	session appobs.ProtocolSessionID,
	lane appobs.LaneIdentity,
	transport appobs.Transport,
) (LaneAdopted, error) {
	value, err := appobs.NewLaneAdopted(WorkflowForCommand(command), session, lane, transport)
	return LaneAdopted{value}, err
}

func NewRelayAvailability(available, total, terminal uint32, everReady bool) (RelayAvailability, error) {
	value, err := appobs.NewRelayAvailability(available, total, terminal, everReady)
	return RelayAvailability{value}, err
}

func NewRelayRecoveryObservation(command Command, authority appobs.RelayAuthority, attempt uint32, state appobs.RelayRecoveryState, failure appobs.Failure, details appobs.RelayRecoveryDetails) (RelayRecovering, error) {
	value, err := appobs.NewRelayRecoveryObservation(WorkflowForCommand(command), authority, attempt, state, failure, details)
	return RelayRecovering{value}, err
}

func NewRelayLifecycleObserved(spec appobs.RelayLifecycleSpec) (RelayLifecycleObserved, error) {
	value, err := appobs.NewRelayLifecycleObserved(spec)
	return RelayLifecycleObserved{value}, err
}

func NewWebRTCLifecycleObserved(spec appobs.WebRTCLifecycleSpec) (WebRTCLifecycleObserved, error) {
	value, err := appobs.NewWebRTCLifecycleObserved(spec)
	return WebRTCLifecycleObserved{value}, err
}

func NewPeerAttemptObserved(spec appobs.PeerAttemptSpec) (PeerAttemptObserved, error) {
	value, err := appobs.NewPeerAttemptObserved(spec)
	return PeerAttemptObserved{value}, err
}

func NewFilesystemOutputObserved(spec fsobs.FilesystemOutputSpec) (FilesystemOutputObserved, error) {
	value, err := fsobs.NewFilesystemOutputObserved(spec)
	return FilesystemOutputObserved{value}, err
}

func NewSenderTerminalSendObserved(
	session appobs.ProtocolSessionID,
	lane appobs.LaneIdentity,
	settled bool,
	transportDisposition appobs.SenderTerminalSendTransport,
	outcome appobs.SenderTerminalSendOutcome,
	decision appobs.SenderTerminalSendDecision,
) (SenderTerminalSendObserved, error) {
	value, err := appobs.NewSenderTerminalSendObserved(session, lane, settled, transportDisposition, outcome, decision)
	return SenderTerminalSendObserved{value}, err
}

func NewCatalogStorageObserved(
	operation appobs.CatalogStorageOperation,
	cause appobs.CatalogStorageCause,
	usage appobs.CatalogUsage,
	legacyRootsRemoved uint64,
) (CatalogStorageObserved, error) {
	value, err := appobs.NewCatalogStorageObserved(operation, cause, usage, legacyRootsRemoved)
	return CatalogStorageObserved{value}, err
}

func NewRootPrefetchObserved(
	decision appobs.RootPrefetchDecision,
	attempt uint64,
	entryCount uint64,
	omittedCount uint64,
) (RootPrefetchObserved, error) {
	value, err := appobs.NewRootPrefetchObserved(decision, attempt, entryCount, omittedCount)
	return RootPrefetchObserved{value}, err
}

func NewNativeConnectivityObserved(spec appobs.NativeConnectivitySpec) (NativeConnectivityObserved, error) {
	value, err := appobs.NewNativeConnectivityObserved(spec)
	return NativeConnectivityObserved{value}, err
}

func NewProtocolOperationObserved(spec appobs.ProtocolOperationSpec) (ProtocolObservationObserved, error) {
	value, err := appobs.NewProtocolOperationObserved(spec)
	return ProtocolObservationObserved{value}, err
}

func NewSenderContentDecisionObserved(context appobs.ProtocolObservationContext, decision appobs.SenderContentDecision, lane appobs.LaneIdentity, hasLane bool) (ProtocolObservationObserved, error) {
	value, err := appobs.NewSenderContentDecisionObserved(context, decision, lane, hasLane)
	return ProtocolObservationObserved{value}, err
}

func NewReceivedProtocolErrorObserved(context appobs.ProtocolObservationContext, content appobs.ProtocolErrorContent, lane appobs.LaneIdentity) (ProtocolObservationObserved, error) {
	value, err := appobs.NewReceivedProtocolErrorObserved(context, content, lane)
	return ProtocolObservationObserved{value}, err
}

func NewResponseSendNotStartedObserved(context appobs.ProtocolObservationContext, sequence uint64, kind appobs.ProtocolMessageKind, content appobs.ProtocolErrorContent, result appobs.ResponseSendResult) (ProtocolObservationObserved, error) {
	value, err := appobs.NewResponseSendNotStartedObserved(context, sequence, kind, content, result)
	return ProtocolObservationObserved{value}, err
}

func NewResponseSendReturnedObserved(context appobs.ProtocolObservationContext, sequence uint64, kind appobs.ProtocolMessageKind, content appobs.ProtocolErrorContent, result appobs.ResponseSendResult) (ProtocolObservationObserved, error) {
	value, err := appobs.NewResponseSendReturnedObserved(context, sequence, kind, content, result)
	return ProtocolObservationObserved{value}, err
}

func NewSendAttemptSettledObserved(context appobs.ProtocolObservationContext, sequence uint64, kind appobs.ProtocolMessageKind, attempt appobs.SendAttemptSnapshot) (ProtocolObservationObserved, error) {
	value, err := appobs.NewSendAttemptSettledObserved(context, sequence, kind, attempt)
	return ProtocolObservationObserved{value}, err
}

func NewSenderCapacityObserved(spec appobs.SenderCapacitySpec) (SenderCapacityObserved, error) {
	value, err := appobs.NewSenderCapacityObserved(spec)
	return SenderCapacityObserved{value}, err
}

func NewSenderRevisionObserved(
	stage appobs.SenderRevisionStage,
	cause appobs.SenderRevisionCause,
	revision appobs.SenderRevisionID,
	lease appobs.RevisionLeaseID,
	session appobs.ProtocolSessionID,
) (SenderRevisionObserved, error) {
	value, err := appobs.NewSenderRevisionObserved(stage, cause, revision, lease, session)
	return SenderRevisionObserved{value}, err
}

func NewSenderSessionTerminated(
	session appobs.ProtocolSessionID,
	trigger appobs.SenderSessionTerminalTrigger,
	provenance appobs.SenderSessionTerminalProvenance,
	failure diagnosticerror.Snapshot,
) (SenderSessionTerminated, error) {
	value, err := appobs.NewSenderSessionTerminated(session, trigger, provenance, failure)
	return SenderSessionTerminated{value}, err
}

func NewTransferLifecycleObserved(spec appobs.TransferLifecycleSpec) (TransferLifecycleObserved, error) {
	value, err := appobs.NewTransferLifecycleObserved(spec)
	return TransferLifecycleObserved{value}, err
}

func commandForWorkflow(workflow appobs.Workflow) Command {
	switch workflow {
	case appobs.WorkflowShare:
		return CommandShare
	case appobs.WorkflowReceive:
		return CommandGet
	default:
		return 0
	}
}
func WorkflowForCommand(command Command) appobs.Workflow {
	switch command {
	case CommandShare:
		return appobs.WorkflowShare
	case CommandGet:
		return appobs.WorkflowReceive
	default:
		return 0
	}
}

// FromObservation adds only CLI presentation behavior to an engine-owned fact.
func FromObservation(value appobs.Event) (Event, error) {
	switch value := value.(type) {

	case appobs.CatalogStorageObserved:
		return CatalogStorageObserved{value}, nil

	case appobs.ContentPathSelected:
		return ContentPathSelected{value}, nil

	case appobs.EngineTaskObserved:
		return EngineTaskObserved{value}, nil

	case appobs.Fallback:
		return Fallback{value}, nil

	case fsobs.FilesystemOutputObserved:
		return FilesystemOutputObserved{value}, nil

	case appobs.LaneAdopted:
		return LaneAdopted{value}, nil

	case appobs.LaneSettlementObserved:
		return LaneSettlementObserved{value}, nil

	case appobs.NativeConnectivityObserved:
		return NativeConnectivityObserved{value}, nil

	case appobs.ObserverLossObserved:
		return ObserverLossObserved{value}, nil

	case appobs.PeerAttemptObserved:
		return PeerAttemptObserved{value}, nil

	case appobs.ProtocolObservationObserved:
		return ProtocolObservationObserved{value}, nil

	case appobs.Ready:
		return Ready{value}, nil

	case appobs.ReceiverTerminationObserved:
		return ReceiverTerminationObserved{value}, nil

	case appobs.RelayAvailability:
		return RelayAvailability{value}, nil

	case appobs.RelayConnected:
		return RelayConnected{value}, nil

	case appobs.RelayLifecycleObserved:
		return RelayLifecycleObserved{value}, nil

	case appobs.RelayRecovering:
		return RelayRecovering{value}, nil

	case appobs.RootPrefetchObserved:
		return RootPrefetchObserved{value}, nil

	case appobs.SenderCapacityObserved:
		return SenderCapacityObserved{value}, nil

	case appobs.SenderRevisionObserved:
		return SenderRevisionObserved{value}, nil

	case appobs.SenderSessionTerminated:
		return SenderSessionTerminated{value}, nil

	case appobs.SenderTerminalSendObserved:
		return SenderTerminalSendObserved{value}, nil

	case appobs.SharingSubjectSelected:
		return SharingSubjectSelected{value}, nil

	case appobs.TransferLifecycleObserved:
		return TransferLifecycleObserved{value}, nil

	case appobs.TransferProgress:
		return TransferProgress{value}, nil

	case appobs.Warning:
		return Warning{value}, nil

	case appobs.WebRTCLifecycleObserved:
		return WebRTCLifecycleObserved{value}, nil

	default:
		return nil, ErrInvalidEvent
	}
}
