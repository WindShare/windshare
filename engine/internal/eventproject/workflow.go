package eventproject

import (
	receive "github.com/windshare/windshare/engine/internal/receive"
	share "github.com/windshare/windshare/engine/internal/share"
	task "github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
)

// Workflow translates implementation observations before they leave the engine.
// Publication remains bounded and nonblocking; rejection is evidence, never a
// decision to stop a transfer or change its authoritative settlement.
func Workflow(workflow appobs.Workflow, envelope task.Observation, publish func(task.Observation) bool) bool {
	return (workflowProjection{workflow: workflow, envelope: envelope, output: publish}).observe(envelope)
}

type workflowProjection struct {
	workflow appobs.Workflow
	envelope task.Observation
	output   func(task.Observation) bool
}

func (p workflowProjection) publish(event task.Event) bool {
	p.envelope.Event = event
	return p.output(p.envelope)
}

func (p workflowProjection) project(category appobs.ObserverLossCategory, value appobs.Event, err error) bool {
	if err == nil {
		return p.publish(value)
	}
	rejection, _ := appobs.NewObserverLossObserved(appobs.ObserverLossSpec{
		Workflow: p.workflow, Category: category, Reason: ObserverLossReason(err),
		Count: 1, Rejection: ProjectionRejection(err),
	})
	return p.publish(rejection)
}

func (p workflowProjection) emit(value appobs.Event, err error) bool {
	return p.project(appobs.ObserverLossEngineObservation, value, err)
}

func (p workflowProjection) observe(envelope task.Observation) bool {
	switch value := envelope.Event.(type) {
	case task.Lifecycle:
		// Wait owns authoritative settlement; the stream carries a product
		// representation of that result and may be dropped under pressure.
		return p.emit(projectTask(p.workflow, envelope))
	case share.Observation:
		return p.share(value)
	case receive.Warning:
		return p.emit(appobs.NewWarning(p.workflow, receiveWarning(value.Code, value.Cause)))
	case receive.RelayConnected:
		authority, err := RelayAuthority(value.Endpoint)
		if err != nil {
			return p.emit(nil, err)
		}
		return p.emit(appobs.NewRelayConnected(p.workflow, authority))
	case receive.RecoveryObserved:
		event, relevant, err := projectReceiverRecovery(value.Value)
		if !relevant {
			return true
		}
		return p.project(appobs.ObserverLossRelayLifecycle, event, err)
	case receive.PeerTerminated:
		return p.receiverTermination(value)
	case receive.ProgressObserved:
		return p.emit(ProjectTransferProgress(value.Operation, value.Job, value.Value, value.Value.CapacityWait.Visible(envelope.At)))
	case receive.ContentPathObserved:
		path := map[receive.ContentPath]appobs.ContentPath{receive.ContentPathRelay: appobs.ContentPathRelay, receive.ContentPathDirect: appobs.ContentPathDirect, receive.ContentPathDirectAndRelay: appobs.ContentPathDirectAndRelay}
		return p.emit(appobs.NewContentPathSelected(path[value.Path]))
	case receive.FallbackObserved:
		return p.emit(appobs.NewFallback(p.workflow, appobs.TransportWebRTC, appobs.TransportRelay, receiveWarning(value.Code, nil)))
	case receive.LaneAdopted:
		session, err := ProtocolSessionID(value.Session)
		if err != nil {
			return p.emit(nil, err)
		}
		lane, err := LaneIdentity(value.Lane)
		if err != nil {
			return p.emit(nil, err)
		}
		return p.emit(appobs.NewLaneAdopted(p.workflow, session, lane, appobs.TransportWebRTC))
	default:
		return p.emit(nil, ErrInvalidProjection)
	}
}

func (p workflowProjection) share(value share.Observation) bool {
	switch {
	case value.RelayAvailability != nil:
		v := value.RelayAvailability
		return p.emit(appobs.NewRelayAvailability(v.Available, v.Total, v.Terminal, v.EverReady))
	case value.RelayRecovery != nil:
		return p.emit(projectSenderRecovery(*value.RelayRecovery))
	default:
		return p.emit(nil, ErrInvalidProjection)
	}
}

func receiveWarning(code receive.FailureCode, cause error) appobs.Failure {
	if mapped, ok := ReceiveFailureCode(code); ok {
		return mustFailure(mapped)
	}
	if failure, ok := ClassifyError(cause); ok {
		return failure
	}
	return mustFailure(appobs.FailureUnexpected)
}

func (p workflowProjection) receiverTermination(value receive.PeerTerminated) bool {
	for _, cause := range value.Value.RetainedCauseClasses() {
		if failure, ok := ProjectReceiverCauseClass(cause); ok {
			return p.emit(appobs.NewWarning(p.workflow, failure))
		}
	}
	if value.Value.PeerShutdownFailed() {
		return p.emit(appobs.NewWarning(p.workflow, mustFailure(appobs.FailurePeerShutdown)))
	}
	if value.Value.ChannelDrainFailed() {
		return p.emit(appobs.NewWarning(p.workflow, mustFailure(appobs.FailurePeerChannelDrain)))
	}
	return true
}
