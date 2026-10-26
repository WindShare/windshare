package clievent

import appobs "github.com/windshare/windshare/engine/observation"

func NewRelayConnected(command Command, origin appobs.RelayOrigin) (appobs.RelayConnected, error) {
	value, err := appobs.NewRelayConnected(WorkflowForCommand(command), origin)
	return value, err
}

func NewRelayRecovering(
	command Command,
	origin appobs.RelayOrigin,
	attempt uint32,
	state appobs.RelayRecoveryState,
	failure appobs.Failure,
) (appobs.RelayRecovering, error) {
	value, err := appobs.NewRelayRecovering(WorkflowForCommand(command), origin, attempt, state, failure)
	return value, err
}

func NewFallback(command Command, from, to appobs.Transport, failure appobs.Failure) (appobs.Fallback, error) {
	value, err := appobs.NewFallback(WorkflowForCommand(command), from, to, failure)
	return value, err
}

func NewWarning(command Command, failure appobs.Failure) (appobs.Warning, error) {
	value, err := appobs.NewWarning(WorkflowForCommand(command), failure)
	return value, err
}

func NewLaneAdopted(
	command Command,
	session appobs.ProtocolSessionID,
	lane appobs.LaneIdentity,
	transport appobs.Transport,
) (appobs.LaneAdopted, error) {
	value, err := appobs.NewLaneAdopted(WorkflowForCommand(command), session, lane, transport)
	return value, err
}

func NewRelayRecoveryObservation(command Command, origin appobs.RelayOrigin, attempt uint32, state appobs.RelayRecoveryState, failure appobs.Failure, details appobs.RelayRecoveryDetails) (appobs.RelayRecovering, error) {
	value, err := appobs.NewRelayRecoveryObservation(WorkflowForCommand(command), origin, attempt, state, failure, details)
	return value, err
}

// CommandOf adds presentation context without wrapping an application fact.
func CommandOf(event Event) Command {
	if value, ok := event.(interface{ Command() Command }); ok {
		return value.Command()
	}
	if value, ok := event.(appobs.Event); ok {
		return commandForWorkflow(value.Workflow())
	}
	return 0
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
