package clievent

func validateCommandFailed(value CommandFailed) error {
	if !value.command.Valid() || !value.exit.Valid() ||
		value.exit == ExitSuccess || !value.failure.Valid() {
		return ErrInvalidEvent
	}
	return nil
}

func validateTransferSettled(value TransferSettled) error {
	if !value.result.Valid() {
		return ErrInvalidEvent
	}
	return nil
}

func validateSharingStopped(value SharingStopped) error {
	if !value.result.Valid() {
		return ErrInvalidEvent
	}
	return nil
}

func validateTraceIncomplete(value TraceIncomplete) error {
	if _, err := NewTraceIncomplete(value.command, value.cause, value.loss); err != nil {
		return err
	}
	return nil
}
