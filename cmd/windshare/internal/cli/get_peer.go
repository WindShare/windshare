package cli

import (
	"context"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
)

const receiverTerminationTraceWaitTime = time.Second

func (a *App) monitorReceiverAdmission(
	admission *relayContentAdmission,
	runtime receiverRuntimeCloser,
) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		decision, ok := <-admission.Decision()
		if !ok || decision.Cause == nil || decision.TerminalOwner != receiverAdmissionTerminalResumeFailed {
			return
		}
		a.logf("get: restore relay content admission failed cause_class=relay_resume")
		if runtime != nil {
			runtime.Close()
		}
	}()
	return done
}

func beginReceiverPlanning(
	startPeer func() *activeReceiverPeer,
	resolveSelection func() (transfer.SelectionRules, error),
) (*activeReceiverPeer, transfer.SelectionRules, error) {
	// downloadT0 and its independent relay deadline are already armed. Starting
	// the peer before bounded rule validation keeps setup concurrent; authenticated
	// --only path traversal belongs to the transfer job and cannot shift T0.
	peer := startPeer()
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
	disposition := receiverPeerFallbackAllowed
	switch outcome.Disposition() {
	case v2peer.ReceiverDispositionSessionUnsafe:
		disposition = receiverPeerSessionUnsafe
	case v2peer.ReceiverDispositionSessionUnavailable:
		disposition = receiverPeerSessionUnavailable
	case v2peer.ReceiverDispositionFallbackAllowed:
		// Local provenance describes who ended the operation; it does not erase a
		// retained cleanup failure that the fallback path still needs to surface.
		if outcome.LocallyCanceled() && retainedCause == nil {
			disposition = receiverPeerLocalStop
		}
	}
	return receiverPeerMonitorOutcome{
		disposition:   disposition,
		retainedCause: retainedCause,
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

type receiverRuntimeCloser interface{ Close() }

type receiverPeerTerminationTrace struct {
	operationID           protocolsession.OperationID
	localGeneration       uint64
	transitionAuthority   v2peer.ReceiverTerminalOwner
	transitionProvenance  v2peer.ReceiverTerminalProvenance
	disposition           v2peer.ReceiverAttemptDisposition
	consequenceProvenance v2peer.ReceiverTerminalProvenance
	diagnosticsTruncated  bool
	benignComponents      []v2peer.ReceiverBenignCause
	retainedCauseClasses  []v2peer.ReceiverCauseClass
	teardownTransitions   []v2peer.PeerTeardownTransition
	peerShutdownFailed    bool
	channelDrainFailed    bool
}

type receiverPeerSetupPhase string

const (
	receiverPeerSetupFactory   receiverPeerSetupPhase = "factory"
	receiverPeerSetupSignaling receiverPeerSetupPhase = "signaling"
	receiverPeerSetupStart     receiverPeerSetupPhase = "start"
)

type activeReceiverPeer struct {
	attempt receiverPeerAttempt
	done    <-chan struct{}
	once    sync.Once
}

func (peer *activeReceiverPeer) Close() {
	if peer == nil {
		return
	}
	peer.once.Do(func() {
		_ = peer.attempt.Close()
		<-peer.done
	})
}

func (a *App) startReceiverPeer(
	ctx context.Context,
	runtime *sessionruntime.ReceiverRuntime,
	observe func(receiverPeerSignal),
) *activeReceiverPeer {
	starter, terminationTraces, err := a.newReceiverPeerStarter()
	if err != nil || starter == nil {
		if err == nil {
			err = v2peer.ErrConfig
		}
		a.logReceiverPeerSetupFailure(receiverPeerSetupFactory, err)
		notifyReceiverPeer(observe, receiverPeerFailed)
		return nil
	}
	signaling, err := v2peer.NewRuntimeReceiverSignaling(runtime)
	if err != nil {
		a.logReceiverPeerSetupFailure(receiverPeerSetupSignaling, err)
		notifyReceiverPeer(observe, receiverPeerFailed)
		return nil
	}
	attempt, err := starter.Start(ctx, signaling, runtime)
	if err != nil || attempt == nil {
		if err == nil {
			err = v2peer.ErrConfig
		}
		a.logReceiverPeerSetupFailure(receiverPeerSetupStart, err)
		notifyReceiverPeer(observe, receiverPeerFailed)
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.monitorReceiverPeer(attempt, runtime, observe)
		a.awaitReceiverTerminationTrace(terminationTraces)
	}()
	return &activeReceiverPeer{attempt: attempt, done: done}
}

func (a *App) logReceiverPeerSetupFailure(phase receiverPeerSetupPhase, cause error) {
	a.logf(
		"get: direct peer setup failed phase=%s cause_class=%s; continuing through relay",
		phase,
		receiverPeerSetupCauseClass(cause),
	)
}

func receiverPeerSetupCauseClass(cause error) v2peer.ReceiverCauseClass {
	classes := v2peer.ReceiverCauseClasses(cause)
	if len(classes) == 0 {
		return v2peer.ReceiverCauseUnknown
	}
	return classes[0]
}

func (a *App) newReceiverPeerStarter() (
	receiverPeerStarter,
	<-chan receiverPeerTerminationTrace,
	error,
) {
	if a.receiverPeerFactory != nil {
		starter, err := a.receiverPeerFactory()
		return starter, nil, err
	}
	terminationTraces := make(chan receiverPeerTerminationTrace, 1)
	factory, err := v2peer.NewReceiverFactory(v2peer.ReceiverFactoryConfig{
		Configuration: v2peer.DefaultConfiguration(),
		OnTermination: func(trace v2peer.ReceiverTerminationTrace) {
			projected := receiverPeerTerminationTrace{
				operationID:           trace.OperationID(),
				localGeneration:       trace.LocalGeneration(),
				transitionAuthority:   trace.TransitionAuthority(),
				transitionProvenance:  trace.TransitionProvenance(),
				disposition:           trace.Disposition(),
				consequenceProvenance: trace.ConsequenceProvenance(),
				diagnosticsTruncated:  trace.DiagnosticsTruncated(),
				benignComponents:      trace.BenignComponents(),
				retainedCauseClasses:  trace.RetainedCauseClasses(),
				teardownTransitions:   trace.TeardownTransitions(),
				peerShutdownFailed:    trace.PeerShutdownFailed(),
				channelDrainFailed:    trace.ChannelDrainFailed(),
			}
			select {
			case terminationTraces <- projected:
			default:
			}
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return receiverPeerFactoryAdapter{factory: factory}, terminationTraces, nil
}

func (a *App) awaitReceiverTerminationTrace(traces <-chan receiverPeerTerminationTrace) {
	if traces == nil {
		return
	}
	timer := time.NewTimer(receiverTerminationTraceWaitTime)
	defer timer.Stop()
	select {
	case trace := <-traces:
		a.logf(
			"get: direct peer termination operation_id=%x local_generation=%d transition_authority=%s transition_provenance=%s disposition=%s consequence_provenance=%s diagnostics_truncated=%t benign_components=%v retained_cause_classes=%v teardown_transitions=%v peer_shutdown_failed=%t channel_drain_failed=%t",
			trace.operationID, trace.localGeneration, trace.transitionAuthority,
			trace.transitionProvenance, trace.disposition, trace.consequenceProvenance,
			trace.diagnosticsTruncated, trace.benignComponents, trace.retainedCauseClasses,
			trace.teardownTransitions, trace.peerShutdownFailed, trace.channelDrainFailed,
		)
	case <-timer.C:
		a.logf("get: direct peer termination trace unavailable cause_class=trace_timeout")
	}
}

func (a *App) monitorReceiverPeer(
	attempt receiverPeerAttempt,
	runtime receiverRuntimeCloser,
	observe func(receiverPeerSignal),
) {
	ready := attempt.Ready()
	attached := false
	for {
		select {
		case <-ready:
			attached = true
			ready = nil
			notifyReceiverPeer(observe, receiverPeerReady)
			if _, ok := attempt.Lane(); ok {
				a.logf("get: direct peer lane active")
			}
		case <-attempt.Done():
			outcome := attempt.Outcome()
			err := outcome.retainedCause
			switch outcome.disposition {
			case receiverPeerSessionUnsafe:
				notifyReceiverPeer(observe, receiverPeerSessionFatal)
				a.logf("get: authenticated peer signaling violated this session; closing the session")
				runtime.Close()
				return
			case receiverPeerSessionUnavailable:
				notifyReceiverPeer(observe, receiverPeerRuntimeTerminal)
				a.logf("get: authenticated runtime ended; direct peer admission stopped")
				return
			case receiverPeerLocalStop:
				return
			}
			_, laneAttached := attempt.Lane()
			if attached || laneAttached {
				notifyReceiverPeer(observe, receiverPeerDetached)
				a.logf("get: direct peer lane lost; continuing on another authenticated path")
			} else {
				notifyReceiverPeer(observe, receiverPeerFailed)
				if err != nil {
					a.logf("get: direct peer connection failed; continuing through relay")
				}
			}
			return
		}
	}
}

func notifyReceiverPeer(observe func(receiverPeerSignal), signal receiverPeerSignal) {
	if observe != nil {
		observe(signal)
	}
}
