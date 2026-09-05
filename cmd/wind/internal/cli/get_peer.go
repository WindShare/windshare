package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/cmd/wind/internal/observationbridge"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2peer/peerset"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/internal/testrun"
)

var ErrInvalidConnectivityPolicy = errors.New("invalid connectivity policy")

// ConnectivityPolicy owns both peer-attempt requirements and application-relay
// content admission. Keeping those decisions together prevents a mode from
// accidentally admitting the content path it promises to exclude.
type ConnectivityPolicy uint8

const (
	ConnectivityAuto ConnectivityPolicy = iota + 1
	ConnectivityRelayOnly
	ConnectivityP2POnly
)

func ParseConnectivityPolicy(value string) (ConnectivityPolicy, error) {
	switch value {
	case "auto":
		return ConnectivityAuto, nil
	case "relay-only":
		return ConnectivityRelayOnly, nil
	case "p2p-only":
		return ConnectivityP2POnly, nil
	default:
		return 0, fmt.Errorf(
			"%w %q; want auto, relay-only, or p2p-only",
			ErrInvalidConnectivityPolicy,
			value,
		)
	}
}

func (policy ConnectivityPolicy) String() string {
	switch policy {
	case ConnectivityAuto:
		return "auto"
	case ConnectivityRelayOnly:
		return "relay-only"
	case ConnectivityP2POnly:
		return "p2p-only"
	default:
		return "invalid"
	}
}

type receiverPeerRequirement uint8

const (
	receiverPeerDisabled receiverPeerRequirement = iota + 1
	receiverPeerPreferred
	receiverPeerRequired
)

type receiverRelayContentMode uint8

const (
	receiverRelayContentImmediate receiverRelayContentMode = iota + 1
	receiverRelayContentProhibited
)

type receiverConnectivityPlan struct {
	peer         receiverPeerRequirement
	relayContent receiverRelayContentMode
}

func (plan receiverConnectivityPlan) valid() bool {
	return plan == (receiverConnectivityPlan{
		peer: receiverPeerPreferred, relayContent: receiverRelayContentImmediate,
	}) || plan == (receiverConnectivityPlan{
		peer: receiverPeerDisabled, relayContent: receiverRelayContentImmediate,
	}) || plan == (receiverConnectivityPlan{
		peer: receiverPeerRequired, relayContent: receiverRelayContentProhibited,
	})
}

func (policy ConnectivityPolicy) receiverPlan() (receiverConnectivityPlan, error) {
	switch policy {
	case ConnectivityAuto:
		return receiverConnectivityPlan{
			peer: receiverPeerPreferred, relayContent: receiverRelayContentImmediate,
		}, nil
	case ConnectivityRelayOnly:
		return receiverConnectivityPlan{
			peer: receiverPeerDisabled, relayContent: receiverRelayContentImmediate,
		}, nil
	case ConnectivityP2POnly:
		return receiverConnectivityPlan{
			peer: receiverPeerRequired, relayContent: receiverRelayContentProhibited,
		}, nil
	default:
		return receiverConnectivityPlan{}, ErrInvalidConnectivityPolicy
	}
}

func (a *App) monitorReceiverAdmission(
	admission receiverContentAdmission,
	runtime receiverRuntimeCloser,
	observation getObservation,
	localStop *receiverLocalStop,
) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		decision, ok := <-admission.Decision()
		if !ok {
			return
		}
		if decision.Cause == nil {
			return
		}
		switch decision.TerminalOwner {
		case receiverAdmissionTerminalResumeFailed:
			observation.warning(decision.Cause)
		case receiverAdmissionTerminalP2PUnavailable:
			observation.warningCode(clievent.FailurePeerStopped)
		default:
			return
		}
		if runtime != nil {
			localStop.record(clievent.ReceiverLocalStopOutputAdmission)
			runtime.Close()
		}
	}()
	return done
}

func (a *App) observeRelayContentAdmission(
	_ receiverAdmissionTrigger,
	_ *receiverContentPaths,
) {
	a.recordProcessTrace(
		processTraceGetComponent,
		processTraceReceiverRelayContent,
		testrun.OutcomeSucceeded,
	)
}

// receiverContentPaths projects recent useful delivery separately from admitted
// connectivity. A ready candidate pair is never evidence of parallel content.
const receiverContentActivityWindow = 2 * time.Second

type receiverContentPaths struct {
	mu               sync.Mutex
	observation      getObservation
	direct           bool
	relay            bool
	published        clievent.ContentPath
	directAdmitted   bool
	hadDirectTraffic bool
	fallbackPending  bool
}

func newReceiverContentPaths(observation getObservation) *receiverContentPaths {
	return &receiverContentPaths{observation: observation}
}
func (paths *receiverContentPaths) observeContent(activity []transfer.LaneContentActivity, now time.Time) {
	if paths == nil {
		return
	}
	paths.mu.Lock()
	defer paths.mu.Unlock()
	wasAdmitted := paths.directAdmitted
	paths.directAdmitted = false
	paths.direct = false
	paths.relay = false
	for _, entry := range activity {
		if entry.Route == transfer.LaneRouteDirect {
			paths.directAdmitted = entry.AdmittedLanes > 0
			paths.hadDirectTraffic = paths.hadDirectTraffic || entry.UsefulBytes > 0
		}
		if entry.UsefulBytes == 0 || entry.LastUsefulAt.IsZero() || now.Sub(entry.LastUsefulAt) > receiverContentActivityWindow {
			continue
		}
		switch entry.Route {
		case transfer.LaneRouteDirect:
			paths.direct = true
		case transfer.LaneRouteRelay, transfer.LaneRouteTURN:
			paths.relay = true
		}
	}
	if wasAdmitted && !paths.directAdmitted && paths.hadDirectTraffic {
		paths.fallbackPending = true
	}
	if paths.fallbackPending && paths.relay {
		paths.fallbackPending = false
		paths.observation.fallback(clievent.FailurePeerStopped)
	}
	if path, changed := paths.changedPathLocked(); changed {
		paths.observation.contentPath(path)
	}
}
func (paths *receiverContentPaths) changedPathLocked() (clievent.ContentPath, bool) {
	var current clievent.ContentPath
	switch {
	case paths.direct && paths.relay:
		current = clievent.ContentPathDirectAndRelay
	case paths.direct:
		current = clievent.ContentPathDirect
	case paths.relay:
		current = clievent.ContentPathRelay
	default:
		return 0, false
	}
	if current == paths.published {
		return current, false
	}
	paths.published = current
	return current, true
}

func beginReceiverPlanning(
	plan receiverConnectivityPlan,
	startPeer func() *activeReceiverPeer,
	admitRelayOnly func() error,
	resolveSelection func() (transfer.SelectionRules, error),
) (*activeReceiverPeer, transfer.SelectionRules, error) {
	if !plan.valid() {
		return nil, transfer.SelectionRules{}, ErrInvalidConnectivityPolicy
	}
	// Content may use the authenticated relay as soon as output authority is
	// ready. Peer discovery and file size never delay that first usable path.
	var peer *activeReceiverPeer
	switch plan.peer {
	case receiverPeerPreferred:
		if err := admitRelayOnly(); err != nil {
			return nil, transfer.SelectionRules{}, err
		}
		peer = startPeer()
	case receiverPeerDisabled:
		if err := admitRelayOnly(); err != nil {
			return nil, transfer.SelectionRules{}, err
		}
	case receiverPeerRequired:
		peer = startPeer()
		if peer == nil {
			return nil, transfer.SelectionRules{}, errReceiverP2PPathUnavailable
		}
	default:
		return nil, transfer.SelectionRules{}, ErrInvalidConnectivityPolicy
	}
	rules, err := resolveSelection()
	return peer, rules, err
}

type receiverPeerAttempt interface {
	Ready() <-chan struct{}
	Done() <-chan struct{}
	Lane() (sessionruntime.LaneIdentity, bool)
	Err() error
	Outcome() receiverPeerMonitorOutcome
	Close() error
}

type receiverPeerDisposition uint8

const (
	receiverPeerFallbackAllowed receiverPeerDisposition = iota + 1
	receiverPeerSessionUnavailable
	receiverPeerSessionUnsafe
	receiverPeerLocalStop
)

type receiverPeerMonitorOutcome struct {
	disposition   receiverPeerDisposition
	retainedCause error
}

type receiverPeerStarter interface {
	Start(
		context.Context,
		v2peer.ReceiverSignaling,
		v2peer.ReceiverLaneSession,
	) (receiverPeerAttempt, error)
}

type receiverPeerOptions struct {
	budget *peerset.Budget
	native *nativepeer.NativePeerConnectivity
	demand peerset.Demand
}

type receiverPeerFactoryAdapter struct {
	options       receiverPeerOptions
	factory       *v2peer.ReceiverFactory
	stopAfterWave bool
}

func (adapter receiverPeerFactoryAdapter) Start(
	ctx context.Context,
	signaling v2peer.ReceiverSignaling,
	lanes v2peer.ReceiverLaneSession,
) (receiverPeerAttempt, error) {
	return adapter.startPeerSet(ctx, signaling, lanes)
}

func (adapter receiverPeerFactoryAdapter) ReceiverTerminationObservations() <-chan v2peer.ReceiverTerminationTrace {
	return adapter.factory.ReceiverTerminationObservations()
}

func (adapter receiverPeerFactoryAdapter) PeerDiagnostics() <-chan v2peer.PeerDiagnosticObservation {
	return adapter.factory.PeerDiagnostics()
}

func (adapter receiverPeerFactoryAdapter) CompleteObservations() v2peer.ReceiverObservationCompletion {
	return adapter.factory.CompleteObservations()
}

type receiverRuntimeCloser interface{ Close() }

type receiverLocalStop struct {
	mu     sync.Mutex
	reason clievent.ReceiverLocalStopReason
}

func (stop *receiverLocalStop) record(reason clievent.ReceiverLocalStopReason) {
	if stop == nil || reason == clievent.ReceiverLocalStopNone {
		return
	}
	stop.mu.Lock()
	if stop.reason == 0 {
		stop.reason = reason
	}
	stop.mu.Unlock()
}

func (stop *receiverLocalStop) snapshot() clievent.ReceiverLocalStopReason {
	if stop == nil {
		return clievent.ReceiverLocalStopNone
	}
	stop.mu.Lock()
	defer stop.mu.Unlock()
	if stop.reason == 0 {
		return clievent.ReceiverLocalStopNone
	}
	return stop.reason
}

type receiverPeerSetupPhase string

const (
	receiverPeerSetupFactory   receiverPeerSetupPhase = "factory"
	receiverPeerSetupSignaling receiverPeerSetupPhase = "signaling"
	receiverPeerSetupStart     receiverPeerSetupPhase = "start"
)

type activeReceiverPeer struct {
	attempt   receiverPeerAttempt
	done      <-chan struct{}
	localStop *receiverLocalStop
	once      sync.Once
}

func (peer *activeReceiverPeer) Close() {
	peer.CloseWithReason(clievent.ReceiverLocalStopCaller)
}

func (peer *activeReceiverPeer) SetDemand(demand peerset.Demand) {
	if peer == nil {
		return
	}
	if owner, ok := peer.attempt.(interface{ SetDemand(peerset.Demand) error }); ok {
		_ = owner.SetDemand(demand)
	}
}

func (peer *activeReceiverPeer) CloseWithReason(reason clievent.ReceiverLocalStopReason) {
	if peer == nil {
		return
	}
	peer.once.Do(func() {
		peer.localStop.record(reason)
		_ = peer.attempt.Close()
		<-peer.done
	})
}

func (a *App) startReceiverPeer(
	ctx context.Context,
	runtime *sessionruntime.ReceiverRuntime,
	observation getObservation,
	observe func(receiverPeerSignal),
	localStop *receiverLocalStop,
	requirement receiverPeerRequirement,
	options ...receiverPeerOptions,
) *activeReceiverPeer {
	starter, err := a.newReceiverPeerStarter(observation, localStop, requirement == receiverPeerRequired, options...)
	if adapter, ok := starter.(receiverPeerFactoryAdapter); ok && len(options) > 0 {
		adapter.options = options[0]
		starter = adapter
	}
	if err != nil || starter == nil {
		observation.warningCode(receiverPeerSetupFailureCode(receiverPeerSetupFactory))
		notifyReceiverPeer(observe, receiverPeerFailed)
		return nil
	}
	signaling, err := v2peer.NewRuntimeReceiverSignaling(runtime)
	if err != nil {
		observation.warningCode(receiverPeerSetupFailureCode(receiverPeerSetupSignaling))
		notifyReceiverPeer(observe, receiverPeerFailed)
		return nil
	}
	attempt, err := starter.Start(ctx, signaling, runtime)
	if err != nil || attempt == nil {
		observation.warningCode(receiverPeerSetupFailureCode(receiverPeerSetupStart))
		notifyReceiverPeer(observe, receiverPeerFailed)
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.monitorReceiverPeer(
			attempt, runtime, runtime.ProtocolSessionID(), observation, observe,
		)
	}()
	return &activeReceiverPeer{attempt: attempt, done: done, localStop: localStop}
}

func receiverPeerSetupFailureCode(phase receiverPeerSetupPhase) clievent.FailureCode {
	switch phase {
	case receiverPeerSetupFactory:
		return clievent.FailurePeerConfiguration
	case receiverPeerSetupSignaling:
		return clievent.FailurePeerSignaling
	default:
		return clievent.FailurePeerNegotiation
	}
}

func (a *App) monitorReceiverPeer(
	attempt receiverPeerAttempt,
	runtime receiverRuntimeCloser,
	session protocolsession.ProtocolSessionID,
	observation getObservation,
	observe func(receiverPeerSignal),
) bool {
	ready := attempt.Ready()
	attached := false
	for {
		select {
		case <-ready:
			attached = true
			ready = nil
			if lane, ok := attempt.Lane(); ok {
				observation.laneAdopted(session, lane)
				a.recordProcessTrace(
					processTraceGetComponent,
					processTraceReceiverDirectLane,
					testrun.OutcomeSucceeded,
				)
			}
			notifyReceiverPeer(observe, receiverPeerReady)
		case <-attempt.Done():
			outcome := attempt.Outcome()
			err := outcome.retainedCause
			switch outcome.disposition {
			case receiverPeerSessionUnsafe:
				notifyReceiverPeer(observe, receiverPeerSessionFatal)
				if err != nil {
					observation.warning(err)
				} else {
					observation.warningCode(clievent.FailurePeerProtocol)
				}
				runtime.Close()
				return false
			case receiverPeerSessionUnavailable:
				notifyReceiverPeer(observe, receiverPeerRuntimeTerminal)
				if err != nil {
					observation.warning(err)
				}
				return false
			case receiverPeerLocalStop:
				return true
			}
			_, laneAttached := attempt.Lane()
			if attached || laneAttached {
				notifyReceiverPeer(observe, receiverPeerDetached)
			} else {
				notifyReceiverPeer(observe, receiverPeerFailed)
			}
			return false
		}
	}
}

func notifyReceiverPeer(observe func(receiverPeerSignal), signal receiverPeerSignal) {
	if observe != nil {
		observe(signal)
	}
}

func (observation getObservation) ObservePeerDiagnostic(value v2peer.PeerDiagnosticObservation) {
	observation.peerDiagnosticContext(context.Background(), nil, value)
}

func (observation getObservation) ObservePeerDiagnosticContext(ctx context.Context, value v2peer.PeerDiagnosticObservation) {
	observation.peerDiagnosticContext(ctx, nil, value)
}

func (observation getObservation) peerDiagnosticContext(
	ctx context.Context,
	gate *observationbridge.PublicationGate,
	value v2peer.PeerDiagnosticObservation,
) {
	if ctx.Err() != nil {
		return
	}
	category, reason, cumulative, err := commandprojection.ProjectPeerDiagnostic(value)
	gate.Commit(ctx, func() bool {
		if err != nil {
			observation.lose(clievent.ObserverLossCommandAdapter, err)
			return false
		}
		observation.reportCumulativeLoss(peerDiagnosticLossSource(value), category, reason, cumulative)
		return true
	})
}

func peerDiagnosticLossSource(value v2peer.PeerDiagnosticObservation) observerLossSource {
	switch value.Category {
	case v2peer.PeerDiagnosticSenderAttempt:
		switch value.Reason {
		case v2peer.PeerDiagnosticStreamCapacity:
			return observerLossSenderAttemptCapacity
		case v2peer.PeerDiagnosticPathCapacity:
			return observerLossSenderPathCapacity
		default:
			return observerLossSenderCleanupResidue
		}
	case v2peer.PeerDiagnosticReceiverTermination:
		return observerLossReceiverTerminationCapacity
	default:
		return 0
	}
}

type receiverPeerSetAdapter struct{ path *peerset.Path }

func (adapter receiverPeerSetAdapter) SetDemand(demand peerset.Demand) error {
	return adapter.path.SetDemand(demand)
}

func (a *receiverPeerSetAdapter) Ready() <-chan struct{}                    { return a.path.Ready() }
func (a *receiverPeerSetAdapter) Done() <-chan struct{}                     { return a.path.Done() }
func (a *receiverPeerSetAdapter) Lane() (sessionruntime.LaneIdentity, bool) { return a.path.Lane() }
func (a *receiverPeerSetAdapter) Err() error                                { return a.path.Err() }
func (a *receiverPeerSetAdapter) Close() error                              { return a.path.Close() }
func (a *receiverPeerSetAdapter) Outcome() receiverPeerMonitorOutcome {
	result := a.path.Result()
	disposition := receiverPeerFallbackAllowed
	if result.Stopped {
		disposition = receiverPeerLocalStop
	} else if result.Scope == protocolsession.PeerFailureSessionTerminal {
		disposition = receiverPeerSessionUnsafe
	}
	return receiverPeerMonitorOutcome{disposition: disposition, retainedCause: result.Cause}
}
func (adapter receiverPeerFactoryAdapter) startPeerSet(ctx context.Context, signaling v2peer.ReceiverSignaling, lanes v2peer.ReceiverLaneSession) (receiverPeerAttempt, error) {
	demand := adapter.options.demand
	if demand == peerset.NoDemand {
		demand = peerset.ContentDemand
	}
	path, err := peerset.OpenReceiver(ctx, peerset.Config{Budget: adapter.options.budget}, peerset.ReceiverConfig{Factory: adapter.factory, Signaling: signaling, Lanes: lanes, Demand: demand, StopAfterWave: adapter.stopAfterWave})
	if err != nil {
		return nil, err
	}
	return &receiverPeerSetAdapter{path: path}, nil
}
