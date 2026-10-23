package clievent

type Visitor interface {
	VisitEngineTaskObserved(EngineTaskObserved) error
	VisitNativeConnectivityObserved(NativeConnectivityObserved) error
	VisitPlatformSetupObserved(PlatformSetupObserved) error
	VisitReady(Ready) error
	VisitSharingSubjectSelected(SharingSubjectSelected) error
	VisitRelayConnected(RelayConnected) error
	VisitRelayRecovering(RelayRecovering) error
	VisitRelayAvailability(RelayAvailability) error
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
	VisitProtocolObservationObserved(ProtocolObservationObserved) error
	VisitLaneSettlementObserved(LaneSettlementObserved) error
	VisitObserverLossObserved(ObserverLossObserved) error
	VisitReceiverTerminationObserved(ReceiverTerminationObserved) error
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
	if visitor == nil {
		return ErrInvalidEvent
	}
	if _, err := NewTraceIncomplete(value.command, value.cause, value.loss); err != nil {
		return err
	}
	return visitor.VisitTraceIncomplete(value)
}
