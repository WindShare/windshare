package clievent

type Visitor interface {
	VisitReady(Ready) error
	VisitSharingSubjectSelected(SharingSubjectSelected) error
	VisitRelayConnected(RelayConnected) error
	VisitRelayRecovering(RelayRecovering) error
	VisitContentPathSelected(ContentPathSelected) error
	VisitFallback(Fallback) error
	VisitTransferProgress(TransferProgress) error
	VisitWarning(Warning) error
	VisitCommandFailed(CommandFailed) error
	VisitTransferSettled(TransferSettled) error
	VisitSharingStopped(SharingStopped) error
	VisitTraceIncomplete(TraceIncomplete) error
	VisitLaneAdopted(LaneAdopted) error
	VisitRelayLifecycleObserved(RelayLifecycleObserved) error
	VisitWebRTCLifecycleObserved(WebRTCLifecycleObserved) error
	VisitPeerAttemptObserved(PeerAttemptObserved) error
	VisitTransferLifecycleObserved(TransferLifecycleObserved) error
	VisitFilesystemOutputObserved(FilesystemOutputObserved) error
	VisitSenderTerminalSendObserved(SenderTerminalSendObserved) error
	VisitSenderSessionTerminated(SenderSessionTerminated) error
	VisitCatalogStorageObserved(CatalogStorageObserved) error
	VisitRootPrefetchObserved(RootPrefetchObserved) error
	VisitSenderCapacityObserved(SenderCapacityObserved) error
	VisitSenderRevisionObserved(SenderRevisionObserved) error
	VisitProtocolOperationObserved(ProtocolOperationObserved) error
	VisitLaneSettlementObserved(LaneSettlementObserved) error
	VisitObserverLossObserved(ObserverLossObserved) error
	VisitReceiverTerminationObserved(ReceiverTerminationObserved) error
}

func acceptReady(visitor Visitor, value Ready) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	return visitor.VisitReady(value)
}

func acceptSharingSubjectSelected(visitor Visitor, value SharingSubjectSelected) error {
	if visitor == nil || !value.subject.Valid() {
		return ErrInvalidEvent
	}
	return visitor.VisitSharingSubjectSelected(value)
}

func acceptRelayConnected(visitor Visitor, value RelayConnected) error {
	if visitor == nil || !value.command.Valid() || !value.authority.Valid() {
		return ErrInvalidEvent
	}
	return visitor.VisitRelayConnected(value)
}

func acceptRelayRecovering(visitor Visitor, value RelayRecovering) error {
	_, stateOK := value.state.Name()
	if visitor == nil || !value.command.Valid() || !value.authority.Valid() ||
		value.attempt == 0 || !stateOK ||
		(value.state == RelayRecoveryFailed) != (value.hasFailure && value.failure.Valid()) {
		return ErrInvalidEvent
	}
	return visitor.VisitRelayRecovering(value)
}

func acceptContentPathSelected(visitor Visitor, value ContentPathSelected) error {
	if visitor == nil || !value.path.Valid() {
		return ErrInvalidEvent
	}
	return visitor.VisitContentPathSelected(value)
}

func acceptFallback(visitor Visitor, value Fallback) error {
	if visitor == nil || !value.command.Valid() || !value.from.Valid() ||
		!value.to.Valid() || value.from == value.to || !value.failure.Valid() {
		return ErrInvalidEvent
	}
	return visitor.VisitFallback(value)
}

func acceptTransferProgress(visitor Visitor, value TransferProgress) error {
	if visitor == nil || !value.receiveOperation.Valid() ||
		!value.transferJob.Valid() || !value.snapshot.Valid() {
		return ErrInvalidEvent
	}
	return visitor.VisitTransferProgress(value)
}

func acceptWarning(visitor Visitor, value Warning) error {
	if visitor == nil || !value.command.Valid() || !value.failure.Valid() {
		return ErrInvalidEvent
	}
	return visitor.VisitWarning(value)
}

func acceptCommandFailed(visitor Visitor, value CommandFailed) error {
	if visitor == nil || !value.command.Valid() || !value.exit.Valid() ||
		value.exit == ExitSuccess || !value.failure.Valid() {
		return ErrInvalidEvent
	}
	return visitor.VisitCommandFailed(value)
}

func acceptTransferSettled(visitor Visitor, value TransferSettled) error {
	if visitor == nil || !value.result.Valid() {
		return ErrInvalidEvent
	}
	return visitor.VisitTransferSettled(value)
}

func acceptSharingStopped(visitor Visitor, value SharingStopped) error {
	if visitor == nil || !value.result.Valid() {
		return ErrInvalidEvent
	}
	return visitor.VisitSharingStopped(value)
}

func acceptTraceIncomplete(visitor Visitor, value TraceIncomplete) error {
	_, causeOK := value.cause.Name()
	if visitor == nil || !value.command.Valid() || !causeOK ||
		value.cause == TraceIncompleteLifecycleDrop && value.lifecycleDrops == 0 {
		return ErrInvalidEvent
	}
	return visitor.VisitTraceIncomplete(value)
}

func acceptLaneAdopted(visitor Visitor, value LaneAdopted) error {
	if visitor == nil || !value.command.Valid() || !value.session.Valid() ||
		!value.lane.Valid() || !value.transport.Valid() {
		return ErrInvalidEvent
	}
	return visitor.VisitLaneAdopted(value)
}

func acceptRelayLifecycleObserved(visitor Visitor, value RelayLifecycleObserved) error {
	if visitor == nil || !validRelayLifecycleSpec(value.spec) {
		return ErrInvalidEvent
	}
	return visitor.VisitRelayLifecycleObserved(value)
}

func acceptWebRTCLifecycleObserved(visitor Visitor, value WebRTCLifecycleObserved) error {
	if visitor == nil || !validWebRTCLifecycleSpec(value.spec) {
		return ErrInvalidEvent
	}
	return visitor.VisitWebRTCLifecycleObserved(value)
}

func acceptPeerAttemptObserved(visitor Visitor, value PeerAttemptObserved) error {
	if visitor == nil || !validPeerAttemptSpec(value.spec) {
		return ErrInvalidEvent
	}
	return visitor.VisitPeerAttemptObserved(value)
}

func acceptTransferLifecycleObserved(visitor Visitor, value TransferLifecycleObserved) error {
	if visitor == nil || !validTransferLifecycleSpec(value.spec) {
		return ErrInvalidEvent
	}
	return visitor.VisitTransferLifecycleObserved(value)
}

func acceptFilesystemOutputObserved(visitor Visitor, value FilesystemOutputObserved) error {
	if visitor == nil || !validFilesystemOutputSpec(value.spec) {
		return ErrInvalidEvent
	}
	return visitor.VisitFilesystemOutputObserved(value)
}

func acceptSenderTerminalSendObserved(visitor Visitor, value SenderTerminalSendObserved) error {
	_, transportOK := value.transportDisposition.Name()
	_, outcomeOK := value.outcome.Name()
	_, decisionOK := value.decision.Name()
	if visitor == nil || !value.session.Valid() || !value.lane.Valid() ||
		!transportOK || !outcomeOK || !decisionOK {
		return ErrInvalidEvent
	}
	return visitor.VisitSenderTerminalSendObserved(value)
}

func acceptSenderSessionTerminated(visitor Visitor, value SenderSessionTerminated) error {
	if visitor == nil || !value.session.Valid() ||
		!validSenderSessionTerminalPair(value.trigger, value.provenance) {
		return ErrInvalidEvent
	}
	return visitor.VisitSenderSessionTerminated(value)
}

func acceptCatalogStorageObserved(visitor Visitor, value CatalogStorageObserved) error {
	_, operationOK := value.operation.Name()
	_, causeOK := value.cause.Name()
	if visitor == nil || !operationOK || !causeOK {
		return ErrInvalidEvent
	}
	return visitor.VisitCatalogStorageObserved(value)
}

func acceptRootPrefetchObserved(visitor Visitor, value RootPrefetchObserved) error {
	_, decisionOK := value.decision.Name()
	if visitor == nil || !decisionOK ||
		(value.decision != RootPrefetchStopped && value.attempt == 0) ||
		(value.decision != RootPrefetchCommitted && (value.entryCount != 0 || value.omittedCount != 0)) {
		return ErrInvalidEvent
	}
	return visitor.VisitRootPrefetchObserved(value)
}

func acceptSenderCapacityObserved(visitor Visitor, value SenderCapacityObserved) error {
	if visitor == nil || !validSenderCapacitySpec(value.spec) {
		return ErrInvalidEvent
	}
	return visitor.VisitSenderCapacityObserved(value)
}

func acceptSenderRevisionObserved(visitor Visitor, value SenderRevisionObserved) error {
	if visitor == nil || !validSenderRevisionObserved(
		value.stage, value.cause, value.revision, value.lease, value.session,
	) {
		return ErrInvalidEvent
	}
	return visitor.VisitSenderRevisionObserved(value)
}

func acceptProtocolOperationObserved(visitor Visitor, value ProtocolOperationObserved) error {
	if visitor == nil || !validProtocolOperationSpec(value.spec) {
		return ErrInvalidEvent
	}
	return visitor.VisitProtocolOperationObserved(value)
}

func acceptLaneSettlementObserved(visitor Visitor, value LaneSettlementObserved) error {
	_, routeOK := value.spec.Route.Name()
	if visitor == nil || !value.spec.Session.Valid() || !value.spec.Lane.Valid() || !routeOK {
		return ErrInvalidEvent
	}
	return visitor.VisitLaneSettlementObserved(value)
}

func acceptObserverLossObserved(visitor Visitor, value ObserverLossObserved) error {
	_, categoryOK := value.spec.Category.Name()
	_, reasonOK := value.spec.Reason.Name()
	if visitor == nil || !value.spec.Command.Valid() || !categoryOK || !reasonOK || value.spec.Count == 0 {
		return ErrInvalidEvent
	}
	return visitor.VisitObserverLossObserved(value)
}

func acceptReceiverTerminationObserved(visitor Visitor, value ReceiverTerminationObserved) error {
	if visitor == nil || !validReceiverTerminationSpec(value.spec) {
		return ErrInvalidEvent
	}
	return visitor.VisitReceiverTerminationObserved(value)
}
