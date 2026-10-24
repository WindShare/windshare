package observation

func validateReady(Ready) error {
	return nil
}

func validateSharingSubjectSelected(value SharingSubjectSelected) error {
	if !value.subject.Valid() {
		return ErrInvalidEvent
	}
	return nil
}

func validateRelayConnected(value RelayConnected) error {
	if !value.workflow.Valid() || !value.authority.Valid() {
		return ErrInvalidEvent
	}
	return nil
}

func validateRelayRecovering(value RelayRecovering) error {
	_, stateOK := value.state.Name()
	if !value.workflow.Valid() || !value.authority.Valid() ||
		value.attempt == 0 || !stateOK ||
		(value.state == RelayRecoveryFailed) != (value.hasFailure && value.failure.Valid()) {
		return ErrInvalidEvent
	}
	return nil
}

func validateContentPathSelected(value ContentPathSelected) error {
	if !value.path.Valid() {
		return ErrInvalidEvent
	}
	return nil
}

func validateFallback(value Fallback) error {
	if !value.workflow.Valid() || !value.from.Valid() ||
		!value.to.Valid() || value.from == value.to || !value.failure.Valid() {
		return ErrInvalidEvent
	}
	return nil
}

func validateTransferProgress(value TransferProgress) error {
	if !value.receiveOperation.Valid() ||
		!value.transferJob.Valid() || !value.snapshot.Valid() {
		return ErrInvalidEvent
	}
	return nil
}

func validateWarning(value Warning) error {
	if !value.workflow.Valid() || !value.failure.Valid() {
		return ErrInvalidEvent
	}
	return nil
}

func validateLaneAdopted(value LaneAdopted) error {
	if !value.workflow.Valid() || !value.session.Valid() ||
		!value.lane.Valid() || !value.transport.Valid() {
		return ErrInvalidEvent
	}
	return nil
}

func validateRelayLifecycleObserved(value RelayLifecycleObserved) error {
	if !validRelayLifecycleSpec(value.spec) {
		return ErrInvalidEvent
	}
	return nil
}

func validateWebRTCLifecycleObserved(value WebRTCLifecycleObserved) error {
	if !validWebRTCLifecycleSpec(value.spec) {
		return ErrInvalidEvent
	}
	return nil
}

func validatePeerAttemptObserved(value PeerAttemptObserved) error {
	if !validPeerAttemptSpec(value.spec) {
		return ErrInvalidEvent
	}
	return nil
}

func validateTransferLifecycleObserved(value TransferLifecycleObserved) error {
	if !validTransferLifecycleSpec(value.spec) {
		return ErrInvalidEvent
	}
	return nil
}

func validateSenderTerminalSendObserved(value SenderTerminalSendObserved) error {
	_, transportOK := value.transportDisposition.Name()
	_, outcomeOK := value.outcome.Name()
	_, decisionOK := value.decision.Name()
	if !value.session.Valid() || !value.lane.Valid() ||
		!transportOK || !outcomeOK || !decisionOK {
		return ErrInvalidEvent
	}
	return nil
}

func validateSenderSessionTerminated(value SenderSessionTerminated) error {
	if !value.session.Valid() ||
		!validSenderSessionTerminalPair(value.trigger, value.provenance) {
		return ErrInvalidEvent
	}
	return nil
}

func validateCatalogStorageObserved(value CatalogStorageObserved) error {
	_, operationOK := value.operation.Name()
	_, causeOK := value.cause.Name()
	if !operationOK || !causeOK {
		return ErrInvalidEvent
	}
	return nil
}

func validateRootPrefetchObserved(value RootPrefetchObserved) error {
	_, decisionOK := value.decision.Name()
	if !decisionOK ||
		(value.decision != RootPrefetchStopped && value.attempt == 0) ||
		(value.decision != RootPrefetchCommitted && (value.entryCount != 0 || value.omittedCount != 0)) {
		return ErrInvalidEvent
	}
	return nil
}

func validateSenderCapacityObserved(value SenderCapacityObserved) error {
	if !validSenderCapacitySpec(value.spec) {
		return ErrInvalidEvent
	}
	return nil
}

func validateSenderRevisionEvent(value SenderRevisionObserved) error {
	if !validSenderRevisionObserved(
		value.stage, value.cause, value.revision, value.lease, value.session,
	) {
		return ErrInvalidEvent
	}
	return nil
}

func validateProtocolObservationObserved(value ProtocolObservationObserved) error {
	if !validProtocolObservation(value) {
		return ErrInvalidEvent
	}
	return nil
}

func validateLaneSettlementObserved(value LaneSettlementObserved) error {
	_, routeOK := value.spec.Route.Name()
	if !value.spec.Session.Valid() || !value.spec.Lane.Valid() || !routeOK {
		return ErrInvalidEvent
	}
	return nil
}

func validateObserverLossObserved(value ObserverLossObserved) error {
	_, categoryOK := value.spec.Category.Name()
	_, reasonOK := value.spec.Reason.Name()
	if !value.spec.Workflow.Valid() || !categoryOK || !reasonOK || value.spec.Count == 0 {
		return ErrInvalidEvent
	}
	return nil
}

func validateReceiverTerminationObserved(value ReceiverTerminationObserved) error {
	if !validReceiverTerminationSpec(value.spec) {
		return ErrInvalidEvent
	}
	return nil
}
