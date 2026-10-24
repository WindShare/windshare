package engine

import (
	"context"
	"slices"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/engine/internal/eventproject"
	"github.com/windshare/windshare/engine/internal/nativeconnectivity"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/relayprofile"
)

type ShareRequest = share.Request
type ShareRelayCandidate = relayprofile.Candidate
type ShareResult = share.Result
type ShareReady struct {
	Capability          link.Link
	SelectedRootSummary liveshare.SelectedRootSummary
	RelayAuthority      appobs.RelayAuthority
}
type ShareObservationLoss = share.ObservationLoss
type ShareObservationSource = share.ObservationSource
type ShareMilestone = share.Milestone

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
	ShareMilestoneReady                   = share.ShareReady
	ShareMilestoneStopping                = share.ShareStopping
	ShareMilestoneStopped                 = share.ShareStopped
	ShareMilestoneSessionRetired          = share.SessionRetired
)

type ShareTask struct {
	*Task[ShareResult]
	controller *share.Controller
}

func (engine *Engine) StartShare(ctx context.Context, request ShareRequest) (*ShareTask, error) {
	controller := share.NewController()
	request.Relays = slices.Clone(request.Relays)
	injected := engine.config.Share
	dependencies := share.Dependencies{
		Controller: controller, RevisionCapacity: engine.revisions.Coordinator(),
		CatalogBudget: engine.catalog, CacheBudget: engine.cache, Connectivity: engine.connectivity,
		Prepare: injected.Prepare, PollNetwork: injected.PollNetwork,
	}
	if injected.Relays != nil {
		dependencies.Relays = func(ctx context.Context, _ []string, _ relayset.SenderFactory) (share.Relays, error) {
			return injected.Relays(ctx, slices.Clone(request.Relays))
		}
	}
	if injected.Peers != nil {
		dependencies.Peers = func(nativeconnectivity.SenderConfig) (share.Peers, error) { return injected.Peers() }
	}
	current, err := startWorkflow(engine, ctx, appobs.WorkflowShare, func(ctx context.Context, control task.Control) task.Completion[share.Result] {
		var stopDiscovery func()
		dependencies.TURN, stopDiscovery = engine.turn.ForShare(ctx, request.Relays)
		defer stopDiscovery()
		dependencies.Control = control
		return share.Run(ctx, request, dependencies)
	})
	if err != nil {
		return nil, err
	}
	return &ShareTask{Task: current, controller: controller}, nil
}

// Ready waits for the first relay registration and local session admission to
// start. No receiver, P2P connection, or directory scan is required. The retained
// result is a startup milestone, not a live availability check; canceling this
// wait does not stop the share. Call StopShare and Wait to stop and join its work.
func (current *ShareTask) Ready(ctx context.Context) (ShareReady, error) {
	value, err := current.controller.Ready(ctx)
	if err != nil {
		return ShareReady{}, err
	}
	authority, err := eventproject.RelayAuthority(value.RelayEndpoint)
	if err != nil {
		return ShareReady{}, err
	}
	return ShareReady{Capability: value.Capability, SelectedRootSummary: value.SelectedRootSummary, RelayAuthority: authority}, nil
}

func (current *ShareTask) StopShare() { current.current.Stop(task.ShareStopped) }
