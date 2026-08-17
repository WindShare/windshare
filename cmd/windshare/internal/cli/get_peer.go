package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/cmd/windshare/internal/commandprojection"
	"github.com/windshare/windshare/cmd/windshare/internal/observationbridge"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/internal/testrun"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
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
	receiverRelayContentAdaptive
	receiverRelayContentProhibited
)

type receiverConnectivityPlan struct {
	peer         receiverPeerRequirement
	relayContent receiverRelayContentMode
}

func (plan receiverConnectivityPlan) valid() bool {
	return plan == (receiverConnectivityPlan{
		peer: receiverPeerPreferred, relayContent: receiverRelayContentAdaptive,
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
			peer: receiverPeerPreferred, relayContent: receiverRelayContentAdaptive,
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
	paths *receiverContentPaths,
) {
	if paths != nil {
		paths.relayAdmitted()
	}
	a.recordProcessTrace(
		processTraceGetComponent,
		processTraceReceiverRelayContent,
		testrun.OutcomeSucceeded,
	)
}

// receiverContentPaths owns the user-visible set of content-capable transports.
// The relay deadline adds a path; it does not prove that an already-admitted
// direct lane failed. Publishing the set prevents that policy transition from
// being rendered as a false P2P timeout.
type receiverContentPaths struct {
	mu          sync.Mutex
	observation getObservation
	direct      bool
	relay       bool
	published   clievent.ContentPath
}

func newReceiverContentPaths(observation getObservation) *receiverContentPaths {
	return &receiverContentPaths{observation: observation}
}

func (paths *receiverContentPaths) observePeer(signal receiverPeerSignal) {
	if paths == nil {
		return
	}
	paths.mu.Lock()
	defer paths.mu.Unlock()
	wasDirect := paths.direct
	switch signal {
	case receiverPeerReady:
		paths.direct = true
	case receiverPeerFailed, receiverPeerDetached:
		paths.direct = false
	default:
		return
	}
	path, changed := paths.changedPathLocked()
	relayAvailable := paths.relay

	// Publication stays inside the state lock so concurrent deadline and peer
	// callbacks cannot expose an older path snapshot after a newer one.
	if wasDirect && signal != receiverPeerReady && relayAvailable {
		paths.observation.fallback(peerPathFailureCode(signal))
	}
	if changed {
		paths.observation.contentPath(path)
	}
}

func (paths *receiverContentPaths) relayAdmitted() {
	if paths == nil {
		return
	}
	paths.mu.Lock()
	defer paths.mu.Unlock()
	paths.relay = true
	path, changed := paths.changedPathLocked()
	if changed {
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

func peerPathFailureCode(signal receiverPeerSignal) clievent.FailureCode {
	if signal == receiverPeerDetached {
		return clievent.FailurePeerStopped
	}
	return clievent.FailurePeerNegotiation
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
	// Any policy-owned relay deadline is already armed. Starting the peer before
	// bounded rule validation keeps setup concurrent; authenticated --only path
	// traversal belongs to the transfer job and cannot shift adaptive policy T0.
	var peer *activeReceiverPeer
	switch plan.peer {
	case receiverPeerPreferred:
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

type receiverPeerFactoryAdapter struct{ factory *v2peer.ReceiverFactory }

type receiverPeerAttemptAdapter struct{ attempt *v2peer.ReceiverAttempt }

func (adapter *receiverPeerAttemptAdapter) Ready() <-chan struct{} { return adapter.attempt.Ready() }
func (adapter *receiverPeerAttemptAdapter) Done() <-chan struct{}  { return adapter.attempt.Done() }
func (adapter *receiverPeerAttemptAdapter) Lane() (sessionruntime.LaneIdentity, bool) {
	return adapter.attempt.Lane()
}
func (adapter *receiverPeerAttemptAdapter) Err() error   { return adapter.attempt.Err() }
func (adapter *receiverPeerAttemptAdapter) Close() error { return adapter.attempt.Close() }
func (adapter *receiverPeerAttemptAdapter) Outcome() receiverPeerMonitorOutcome {
	outcome := adapter.attempt.Outcome()
	retainedCause := outcome.RetainedCause()
	locallyCanceled := outcome.LocallyCanceled()
	disposition := receiverPeerFallbackAllowed
	switch outcome.Disposition() {
	case v2peer.ReceiverDispositionSessionUnsafe:
		disposition = receiverPeerSessionUnsafe
	case v2peer.ReceiverDispositionSessionUnavailable:
		disposition = receiverPeerSessionUnavailable
	case v2peer.ReceiverDispositionFallbackAllowed:
		// Local authority owns settlement even when teardown retains diagnostic
		// residue; cleanup cannot retroactively turn an explicit stop into fallback.
		if locallyCanceled {
			disposition = receiverPeerLocalStop
		}
	}
	return receiverPeerMonitorOutcome{
		disposition: disposition, retainedCause: retainedCause,
	}
}

func (adapter receiverPeerFactoryAdapter) Start(
	ctx context.Context,
	signaling v2peer.ReceiverSignaling,
	lanes v2peer.ReceiverLaneSession,
) (receiverPeerAttempt, error) {
	attempt, err := adapter.factory.Start(ctx, signaling, lanes)
	if err != nil || attempt == nil {
		return nil, err
	}
	return &receiverPeerAttemptAdapter{attempt: attempt}, nil
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
) *activeReceiverPeer {
	starter, err := a.newReceiverPeerStarter(observation, localStop)
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

func (a *App) newReceiverPeerStarter(
	observation getObservation,
	localStop *receiverLocalStop,
) (receiverPeerStarter, error) {
	if a.receiverPeerFactory != nil {
		starter, err := a.receiverPeerFactory()
		if err == nil {
			if completer, ok := starter.(receiverObservationCompleter); ok {
				observation.registerReceiverFactory(completer, localStop)
			}
		}
		return starter, err
	}
	if observation.runtime == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		factory, err := v2peer.NewReceiverFactory(v2peer.ReceiverFactoryConfig{
			Configuration: v2peer.DefaultConfiguration(),
			DataChannels: v2peer.DataChannelAdapterFunc(func(channel *pion.DataChannel) (v2peer.PeerDataChannel, error) {
				return transportwebrtc.NewChannelWithOptions(channel, transportwebrtc.ChannelOptions{})
			}),
		})
		if err != nil {
			return nil, err
		}
		return receiverPeerFactoryAdapter{factory: factory}, nil
	}
	factory, err := v2peer.NewReceiverFactory(v2peer.ReceiverFactoryConfig{
		Configuration: v2peer.DefaultConfiguration(),
		DataChannels: v2peer.DataChannelAdapterFunc(func(channel *pion.DataChannel) (v2peer.PeerDataChannel, error) {
			wrapped, wrapErr := transportwebrtc.NewChannelWithOptions(channel, transportwebrtc.ChannelOptions{
				LifecycleObservationCapacity: observation.webRTCObservationCapacity(),
			})
			if wrapErr == nil {
				observation.webRTCObservationSet().registerReceiver(wrapped, observation)
			}
			return wrapped, wrapErr
		}),
		ReceiverTerminationObservationCapacity: v2peer.DefaultReceiverTerminationObservationCapacity,
		PeerDiagnosticObservationCapacity:      v2peer.DefaultPeerDiagnosticObservationCapacity,
	})
	if err != nil {
		return nil, err
	}
	adapter := receiverPeerFactoryAdapter{factory: factory}
	observation.registerReceiverFactory(adapter, localStop)
	return adapter, nil
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
		case v2peer.PeerDiagnosticEvidenceCapacity:
			return observerLossSenderEvidenceCapacity
		default:
			return observerLossSenderCleanupResidue
		}
	case v2peer.PeerDiagnosticReceiverTermination:
		return observerLossReceiverTerminationCapacity
	default:
		return 0
	}
}
