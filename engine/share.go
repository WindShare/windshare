package engine

import (
	"context"
	"slices"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/engine/internal/eventproject"
	"github.com/windshare/windshare/engine/internal/nativeconnectivity"
	"github.com/windshare/windshare/engine/internal/networkruntime"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/engine/networkconfig"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/relayprofile"
)

var ErrInvalidShareChunkSize = share.ErrInvalidChunkSize

type ShareRequest struct {
	Source      liveshare.FileSourceFactory
	RelayIntent relayprofile.Intent
	Frontend    string
	ChunkSize   uint32
	Diagnostics diagnostics.Sink
}
type ShareRelayCandidate = relayprofile.Candidate
type ShareResult = share.Result
type ShareReady struct {
	Capability          link.Link
	SelectedRootSummary liveshare.SelectedRootSummary
	RelayOrigin         appobs.RelayOrigin
	FrozenCandidates    []networkconfig.Candidate
	InitialAdmissions   []string
	Frontend            string
	Decision            networkconfig.Decision
}
type ShareTask struct {
	*Task[ShareResult]
	controller *share.Controller
	selection  networkruntime.Selection
}

func (engine *Engine) StartShare(ctx context.Context, request ShareRequest) (*ShareTask, error) {
	if ctx == nil {
		return nil, share.ErrConfiguration
	}
	engine.mu.Lock()
	closed := engine.closing
	engine.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	selection, err := engine.network.Plan(ctx, request.RelayIntent, request.Frontend)
	if err != nil {
		return nil, err
	}
	controller := share.NewController()
	execution := share.Request{Source: request.Source, Relays: selection.Relays, ChunkSize: request.ChunkSize, Diagnostics: request.Diagnostics}
	admissions := relayset.NewNetworkAdmissions(selection.Plan)
	injected := engine.config.Share
	dependencies := share.Dependencies{
		Controller: controller, RevisionCapacity: engine.revisions.Coordinator(),
		CatalogBudget: engine.catalog, CacheBudget: engine.cache, Connectivity: engine.connectivity,
		Prepare: injected.Prepare, PollNetwork: injected.PollNetwork,
		Attempts: admissions.Control,
		ICEPool:  engine.network.ICEPool,
	}
	if injected.Relays != nil {
		dependencies.Relays = func(ctx context.Context, _ []string, _ relayset.SenderFactory) (share.Relays, error) {
			return injected.Relays(ctx, slices.Clone(execution.Relays))
		}
	}
	if injected.Peers != nil {
		dependencies.Peers = func(nativeconnectivity.SenderConfig) (share.Peers, error) { return injected.Peers() }
	}
	current, err := startWorkflow(engine, ctx, appobs.WorkflowShare, request.Diagnostics, func(ctx context.Context, control task.Control) task.Completion[share.Result] {
		control.Diagnostics.Record(selection)
		dependencies.ObserveAttempt = engine.network.Observer(control.Diagnostics)
		admissions.Observe(func(value relayset.Admission) { control.Diagnostics.Emit(networkruntime.AdmissionRecord(value)) })
		var stopDiscovery func()
		dependencies.TURN, stopDiscovery = engine.turn.ForShare(ctx, execution.Relays)
		defer stopDiscovery()
		dependencies.Control = control
		execution.Diagnostics = control.Diagnostics
		return share.Run(ctx, execution, dependencies)
	})
	if err != nil {
		return nil, err
	}
	return &ShareTask{Task: current, controller: controller, selection: selection}, nil
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
	origin, err := eventproject.RelayOrigin(value.RelayEndpoint)
	if err != nil {
		return ShareReady{}, err
	}
	if len(current.selection.Plan.Network) > 0 {
		value.Capability.Relays = nil
		value.Capability.NodeAliases = nil
		for _, candidate := range current.selection.Plan.Configured {
			value.Capability.Relays = append(value.Capability.Relays, candidate.URL)
		}
		for _, candidate := range current.selection.Plan.Network {
			value.Capability.NodeAliases = append(value.Capability.NodeAliases, candidate.Alias)
		}
	}
	decision := current.selection.Decision
	decision.Candidates, decision.Filters = slices.Clone(decision.Candidates), slices.Clone(decision.Filters)
	return ShareReady{Capability: value.Capability, SelectedRootSummary: value.SelectedRootSummary, RelayOrigin: origin,
		FrozenCandidates: slices.Clone(decision.Candidates), InitialAdmissions: slices.Clone(value.Admissions), Frontend: current.selection.Frontend, Decision: decision}, nil
}

func (current *ShareTask) StopShare() { current.current.Stop(task.ShareStopped) }
