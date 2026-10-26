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
	if !value.workflow.Valid() || !value.origin.Valid() {
		return ErrInvalidEvent
	}
	return nil
}

func validateRelayRecovering(value RelayRecovering) error {
	_, stateOK := value.state.Name()
	if !value.workflow.Valid() || !value.origin.Valid() ||
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

func validateObserverLossObserved(value ObserverLossObserved) error {
	_, categoryOK := value.spec.Category.Name()
	_, reasonOK := value.spec.Reason.Name()
	if !value.spec.Workflow.Valid() || !categoryOK || !reasonOK || value.spec.Count == 0 {
		return ErrInvalidEvent
	}
	return nil
}
