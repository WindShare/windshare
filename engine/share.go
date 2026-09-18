package engine

import (
	"context"

	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
)

type ShareRequest = share.Request
type ShareResult = share.Result
type ShareReady = share.Ready
type ShareObservation = share.Observation
type ShareObservationLoss = share.ObservationLoss
type ShareObservationSource = share.ObservationSource
type ShareMilestone = share.Milestone
type SharePublicationError = share.PublicationError
type SharePublicationStage = share.PublicationStage

const (
	ShareLossProtocol                     = share.ProtocolObservations
	ShareLossNative                       = share.NativeObservations
	ShareLossRelay                        = share.RelayObservations
	ShareLossWebRTC                       = share.WebRTCObservations
	ShareLossSenderAttempt                = share.SenderAttempts
	ShareLossPeerDiagnostic               = share.PeerDiagnostics
	ShareMilestoneSourceAcquiring         = share.SourceAcquiring
	ShareMilestoneSourceAcquired          = share.SourceAcquired
	ShareMilestoneSourceAcquisitionFailed = share.SourceAcquisitionFailed
	ShareMilestoneActivated               = share.ShareActivated
	ShareMilestoneStopping                = share.ShareStopping
	ShareMilestoneStopped                 = share.ShareStopped
	ShareMilestoneSessionRetired          = share.SessionRetired
	SharePublicationEncoding              = share.PublicationEncoding
	SharePublicationOutput                = share.PublicationOutput
	SharePublicationReadiness             = share.PublicationReadiness
)

type ShareTask struct {
	*Task[ShareResult]
	controller *share.Controller
}

func (engine *Engine) StartShare(ctx context.Context, request ShareRequest) (*ShareTask, error) {
	controller := share.NewController()
	request.RelayURLs = append([]string(nil), request.RelayURLs...)
	injected := engine.config.Share
	dependencies := share.Dependencies{
		Controller: controller, RevisionCapacity: engine.revisions.Coordinator(),
		CatalogBudget: engine.catalog, CacheBudget: engine.cache,
		Prepare: injected.Prepare, Relays: injected.Relays, Relay: injected.Relay,
		Peers: injected.Peers, PollNetwork: injected.PollNetwork,
	}
	current, err := start(engine, ctx, func(ctx context.Context, control task.Control) task.Completion[share.Result] {
		dependencies.Control = control
		return share.Run(ctx, request, dependencies)
	})
	if err != nil {
		return nil, err
	}
	return &ShareTask{Task: current, controller: controller}, nil
}

func (current *ShareTask) Ready(ctx context.Context) (ShareReady, error) {
	return current.controller.Ready(ctx)
}

func (current *ShareTask) Activate(publicationErr error) {
	current.controller.Acknowledge(publicationErr)
}

func (current *ShareTask) Activated(ctx context.Context) error {
	return current.controller.Activated(ctx)
}

func (current *ShareTask) StopShare() { current.current.Stop(task.ShareStopped) }
