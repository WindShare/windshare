package eventproject

import (
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
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
		// Lifecycle is also the application's state API; the detailed fact adds
		// diagnostic correlation without asking clients to infer settlement.
		accepted := p.publish(value)
		return p.emit(projectTask(p.workflow, envelope)) && accepted
	case share.Observation:
		return p.share(envelope, value)
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
	case receive.RelayObserved:
		event, err := ProjectRelayLifecycle(p.workflow, value.Value)
		accepted := p.project(appobs.ObserverLossRelayLifecycle, event, err)
		if err == nil && event.Stage() == appobs.RelayTraceDropped {
			p.publish(appobs.LossSnapshot{Source: appobs.LossRelayQueue, Category: appobs.ObserverLossRelayLifecycle, Reason: appobs.ObserverLossTraceQueue, Count: event.Dropped()})
		}
		return accepted
	case receive.WebRTCObserved:
		event, err := ProjectWebRTCLifecycle(p.workflow, value.Value)
		accepted := p.project(appobs.ObserverLossWebRTCLifecycle, event, err)
		if err == nil && event.Transition() == appobs.WebRTCTraceDropped {
			p.publish(appobs.LossSnapshot{Source: appobs.LossWebRTCQueue, Category: appobs.ObserverLossWebRTCLifecycle, Reason: appobs.ObserverLossTraceQueue, Count: event.Dropped()})
		}
		return accepted
	case receive.FilesystemObserved:
		event, err := ProjectFilesystemOutput(value.Value)
		return p.project(appobs.ObserverLossFilesystemOutput, event, err)
	case receive.TransferObserved:
		event, err := ProjectTransferLifecycle(value.Value)
		return p.project(appobs.ObserverLossTransferLifecycle, event, err)
	case receive.ProtocolObserved:
		event, err := ProjectProtocolObservation(p.workflow, value.Value)
		return p.project(appobs.ObserverLossProtocolOperation, event, err)
	case receive.LaneObserved:
		event, err := ProjectLaneSettlement(value.Value)
		return p.project(appobs.ObserverLossLaneSettlement, event, err)
	case receive.NativeObserved:
		event, err := ProjectNativeObservation(p.workflow, value.Value)
		return p.project(appobs.ObserverLossNativeConnectivity, event, err)
	case receive.PeerDiagnosticObserved:
		return projectPeerLoss(value.Value, p.publish, p.emit)
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
	case receive.ObservationLoss:
		return p.publish(ReceiveLoss(value))
	case receive.GenerationChanged, receive.AdmissionObserved:
		return p.emit(projectTask(p.workflow, envelope))
	case receive.Milestone:
		return p.publish(value)
	default:
		return p.emit(nil, ErrInvalidProjection)
	}
}

func (p workflowProjection) share(envelope task.Observation, value share.Observation) bool {
	switch {
	case value.Catalog != nil:
		event, err := ProjectCatalogStorage(*value.Catalog)
		return p.project(appobs.ObserverLossCatalogStorage, event, err)
	case value.Prefetch != nil:
		event, err := ProjectRootPrefetch(*value.Prefetch)
		return p.project(appobs.ObserverLossRootPrefetch, event, err)
	case value.Revision != nil:
		event, err := ProjectSenderRevision(*value.Revision)
		return p.project(appobs.ObserverLossSenderRevision, event, err)
	case value.RelayAvailability != nil:
		v := value.RelayAvailability
		return p.emit(appobs.NewRelayAvailability(v.Available, v.Total, v.Terminal, v.EverReady))
	case value.RelayRecovery != nil:
		return p.emit(projectSenderRecovery(*value.RelayRecovery))
	case value.RelayLifecycle != nil:
		event, err := ProjectRelayLifecycle(p.workflow, *value.RelayLifecycle)
		accepted := p.project(appobs.ObserverLossRelayLifecycle, event, err)
		if err == nil && event.Stage() == appobs.RelayTraceDropped {
			p.publish(appobs.LossSnapshot{Source: appobs.LossRelayQueue, Category: appobs.ObserverLossRelayLifecycle, Reason: appobs.ObserverLossTraceQueue, Count: event.Dropped()})
		}
		return accepted
	case value.SenderAttempt != nil:
		event, err := ProjectSenderAttempt(p.workflow, *value.SenderAttempt)
		return p.project(appobs.ObserverLossSenderAttempt, event, err)
	case value.PeerDiagnostic != nil:
		return projectPeerLoss(*value.PeerDiagnostic, p.publish, p.emit)
	case value.Native != nil:
		event, err := ProjectNativeObservation(p.workflow, *value.Native)
		return p.project(appobs.ObserverLossNativeConnectivity, event, err)
	case value.WebRTC != nil:
		event, err := ProjectWebRTCLifecycle(p.workflow, *value.WebRTC)
		accepted := p.project(appobs.ObserverLossWebRTCLifecycle, event, err)
		if err == nil && event.Transition() == appobs.WebRTCTraceDropped {
			p.publish(appobs.LossSnapshot{Source: appobs.LossWebRTCQueue, Category: appobs.ObserverLossWebRTCLifecycle, Reason: appobs.ObserverLossTraceQueue, Count: event.Dropped()})
		}
		return accepted
	case value.Protocol != nil:
		event, err := ProjectProtocolObservation(p.workflow, *value.Protocol)
		return p.project(appobs.ObserverLossProtocolOperation, event, err)
	case value.TerminalSend != nil:
		event, err := ProjectSenderTerminalSend(*value.TerminalSend)
		return p.project(appobs.ObserverLossSenderTerminalSend, event, err)
	case value.SessionTerminal != nil:
		event, err := ProjectSenderSessionTerminated(*value.SessionTerminal)
		return p.project(appobs.ObserverLossSenderSessionTerminal, event, err)
	case value.Loss != nil:
		return p.publish(ShareLoss(*value.Loss))
	case value.Milestone == share.SourceAcquiring || value.Milestone == share.SourceAcquired || value.Milestone == share.SourceAcquisitionFailed:
		return p.emit(projectTask(p.workflow, envelope))
	case value.Milestone == share.ShareReady || value.Milestone == share.ShareStopping || value.Milestone == share.ShareStopped || value.Milestone == share.SessionRetired:
		// These milestones are application facts, not connection implementations.
		return p.publish(ShareMilestone{Milestone: value.Milestone, Failed: value.Failure != nil})
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

type ShareMilestone struct {
	Milestone share.Milestone
	Failed    bool
}

func (ShareMilestone) EngineEvent() {}

func projectLocalStop(value receive.ReceiverLocalStopReason) appobs.ReceiverLocalStopReason {
	switch value {
	case receive.ReceiverLocalStopCaller:
		return appobs.ReceiverLocalStopCaller
	case receive.ReceiverLocalStopOutputAdmission:
		return appobs.ReceiverLocalStopOutputAdmission
	case receive.ReceiverLocalStopRuntimeSessionFailure:
		return appobs.ReceiverLocalStopRuntimeSessionFailure
	case receive.ReceiverLocalStopNormalCompletion:
		return appobs.ReceiverLocalStopNormalCompletion
	default:
		return appobs.ReceiverLocalStopNone
	}
}

func (p workflowProjection) receiverTermination(value receive.PeerTerminated) bool {
	event, err := ProjectReceiverTermination(value.Value, projectLocalStop(value.LocalStop))
	accepted := p.project(appobs.ObserverLossReceiverTermination, event, err)
	if err != nil {
		return accepted
	}
	for _, cause := range value.Value.RetainedCauseClasses() {
		if failure, ok := ProjectReceiverCauseClass(cause); ok {
			p.emit(appobs.NewWarning(p.workflow, failure))
			return accepted
		}
	}
	if value.Value.PeerShutdownFailed() {
		p.emit(appobs.NewWarning(p.workflow, mustFailure(appobs.FailurePeerShutdown)))
	} else if value.Value.ChannelDrainFailed() {
		p.emit(appobs.NewWarning(p.workflow, mustFailure(appobs.FailurePeerChannelDrain)))
	}
	return accepted
}
