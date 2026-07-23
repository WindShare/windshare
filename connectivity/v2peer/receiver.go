package v2peer

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

type ReceiverPeerConnection interface {
	OnICECandidate(func(*pion.ICECandidate))
	OnConnectionStateChange(func(pion.PeerConnectionState))
	OnDataChannel(func(*pion.DataChannel))
	CreateDataChannel(string, *pion.DataChannelInit) (*pion.DataChannel, error)
	CreateOffer(*pion.OfferOptions) (pion.SessionDescription, error)
	SetLocalDescription(pion.SessionDescription) error
	LocalDescription() *pion.SessionDescription
	SetRemoteDescription(pion.SessionDescription) error
	AddICECandidate(pion.ICECandidateInit) error
	Close() error
}

type ReceiverPeerConnectionFactory interface {
	NewReceiverPeerConnection(pion.Configuration) (ReceiverPeerConnection, error)
}

type ReceiverPeerConnectionFactoryFunc func(pion.Configuration) (ReceiverPeerConnection, error)

func (function ReceiverPeerConnectionFactoryFunc) NewReceiverPeerConnection(
	configuration pion.Configuration,
) (ReceiverPeerConnection, error) {
	if function == nil {
		return nil, ErrConfig
	}
	return function(configuration)
}

type ReceiverAttemptTimer interface {
	C() <-chan time.Time
	Stop()
}

type ReceiverAttemptTimerSource interface {
	NewReceiverAttemptTimer(time.Duration) (ReceiverAttemptTimer, error)
}

type systemReceiverAttemptTimer struct{ timer *time.Timer }

func (timer systemReceiverAttemptTimer) C() <-chan time.Time { return timer.timer.C }

func (timer systemReceiverAttemptTimer) Stop() {
	if timer.timer == nil || timer.timer.Stop() {
		return
	}
	select {
	case <-timer.timer.C:
	default:
	}
}

type systemReceiverAttemptTimerSource struct{}

func (systemReceiverAttemptTimerSource) NewReceiverAttemptTimer(
	duration time.Duration,
) (ReceiverAttemptTimer, error) {
	return systemReceiverAttemptTimer{timer: time.NewTimer(duration)}, nil
}

type ReceiverLaneSession interface {
	RequestLane(context.Context, uint32) (sessionruntime.LaneAttachmentGrant, error)
	AttachLane(
		context.Context,
		sessionruntime.LaneAttachmentGrant,
		protocolsession.FrameChannel,
	) (sessionruntime.LaneIdentity, error)
}

type ReceiverFactoryConfig struct {
	Configuration   pion.Configuration
	PeerConnections ReceiverPeerConnectionFactory
	DataChannels    DataChannelAdapter
	AttemptTimeout  time.Duration
	AttemptTimers   ReceiverAttemptTimerSource
	MaxCandidates   int
	Random          io.Reader
	OnTermination   func(ReceiverTerminationTrace)
}

type ReceiverFactory struct {
	configuration   pion.Configuration
	peerConnections ReceiverPeerConnectionFactory
	dataChannels    DataChannelAdapter
	attemptTimeout  time.Duration
	attemptTimers   ReceiverAttemptTimerSource
	maxCandidates   int
	random          io.Reader
	onTermination   func(ReceiverTerminationTrace)
	readMu          sync.Mutex
}

func NewReceiverFactory(config ReceiverFactoryConfig) (*ReceiverFactory, error) {
	if config.AttemptTimeout < 0 || config.MaxCandidates < 0 ||
		config.MaxCandidates > maximumConfiguredCandidates {
		return nil, ErrConfig
	}
	if config.AttemptTimeout == 0 {
		config.AttemptTimeout = DefaultAttemptTimeout
	}
	if config.MaxCandidates == 0 {
		config.MaxCandidates = DefaultMaxCandidates
	}
	if config.AttemptTimers == nil {
		config.AttemptTimers = systemReceiverAttemptTimerSource{}
	}
	if config.PeerConnections == nil {
		config.PeerConnections = ReceiverPeerConnectionFactoryFunc(
			func(configuration pion.Configuration) (ReceiverPeerConnection, error) {
				return pion.NewPeerConnection(configuration)
			},
		)
	}
	if config.DataChannels == nil {
		config.DataChannels = DataChannelAdapterFunc(func(channel *pion.DataChannel) (PeerDataChannel, error) {
			return transportwebrtc.NewChannel(channel)
		})
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.OnTermination == nil {
		config.OnTermination = func(ReceiverTerminationTrace) {}
	}
	return &ReceiverFactory{
		configuration: config.Configuration, peerConnections: config.PeerConnections,
		dataChannels: config.DataChannels, attemptTimeout: config.AttemptTimeout,
		attemptTimers: config.AttemptTimers,
		maxCandidates: config.MaxCandidates, random: config.Random,
		onTermination: config.OnTermination,
	}, nil
}

type ReceiverAttempt struct {
	factory   *ReceiverFactory
	signaling ReceiverSignaling
	lanes     ReceiverLaneSession
	peer      ReceiverPeerConnection
	channel   PeerDataChannel
	binding   v2signal.Binding
	events    chan receiverEvent

	ctx     context.Context
	cancel  context.CancelCauseFunc
	done    chan struct{}
	ready   chan struct{}
	inboxMu sync.Mutex
	closed  bool

	resultMu sync.Mutex
	result   error
	outcome  ReceiverAttemptOutcome
	lane     sessionruntime.LaneIdentity

	signalingMu       sync.Mutex
	operation         *receiverBoundSignalingOperation
	shutdownRequested bool
	shutdownDecision  receiverAttemptDecision
	deadline          ReceiverAttemptTimer
}

func (factory *ReceiverFactory) Start(
	parent context.Context,
	signaling ReceiverSignaling,
	lanes ReceiverLaneSession,
) (*ReceiverAttempt, error) {
	if factory == nil || signaling == nil || lanes == nil || parent == nil {
		return nil, ErrConfig
	}
	binding, err := factory.newBinding()
	if err != nil {
		return nil, err
	}
	peer, err := factory.peerConnections.NewReceiverPeerConnection(factory.configuration)
	if err != nil || peer == nil {
		return nil, errors.Join(ErrNegotiation, err)
	}
	raw, err := peer.CreateDataChannel(
		transportwebrtc.ChannelLabel,
		transportwebrtc.DefaultDataChannelInit(),
	)
	if err != nil || raw == nil {
		teardown := teardownPeerTransport(peer, nil)
		return nil, errors.Join(ErrNegotiation, err, teardown.cause())
	}
	channel, err := factory.dataChannels.WrapDataChannel(raw)
	if err != nil || channel == nil {
		teardown := teardownPeerTransport(peer, raw)
		return nil, errors.Join(errChannelAdmission, err, teardown.cause())
	}
	deadline, err := factory.attemptTimers.NewReceiverAttemptTimer(factory.attemptTimeout)
	if err != nil || deadline == nil {
		teardown := teardownPeerTransport(peer, channel)
		return nil, errors.Join(ErrConfig, err, teardown.cause())
	}
	ctx, cancel := context.WithCancelCause(parent)
	attempt := &ReceiverAttempt{
		factory: factory, signaling: signaling, lanes: lanes, peer: peer, channel: channel, binding: binding,
		events: make(chan receiverEvent, factory.maxCandidates*2+attemptEventReserve),
		ctx:    ctx, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}),
		deadline: deadline,
	}
	attempt.registerCallbacks()
	go attempt.run()
	return attempt, nil
}

func (factory *ReceiverFactory) newBinding() (v2signal.Binding, error) {
	factory.readMu.Lock()
	defer factory.readMu.Unlock()
	pathID, err := readReceiverSignalID[v2signal.PeerPathID](factory.random)
	if err != nil {
		return v2signal.Binding{}, err
	}
	attemptID, err := readReceiverSignalID[v2signal.AttemptID](factory.random)
	if err != nil {
		return v2signal.Binding{}, err
	}
	return v2signal.Binding{PeerPathID: pathID, AttemptID: attemptID}, nil
}

func readReceiverSignalID[T ~[v2signal.IdentityBytes]byte](source io.Reader) (T, error) {
	for attempt := 0; attempt < 4; attempt++ {
		var identity T
		if _, err := io.ReadFull(source, identity[:]); err != nil {
			return T{}, err
		}
		var zero T
		if identity != zero {
			return identity, nil
		}
	}
	return T{}, ErrConfig
}

func (attempt *ReceiverAttempt) Ready() <-chan struct{} { return attempt.ready }
func (attempt *ReceiverAttempt) Done() <-chan struct{}  { return attempt.done }

func (attempt *ReceiverAttempt) Lane() (sessionruntime.LaneIdentity, bool) {
	if attempt == nil {
		return sessionruntime.LaneIdentity{}, false
	}
	attempt.resultMu.Lock()
	defer attempt.resultMu.Unlock()
	return attempt.lane, attempt.lane.ID != 0 && attempt.lane.Epoch != 0
}

func (attempt *ReceiverAttempt) Err() error {
	if attempt == nil {
		return ErrConfig
	}
	attempt.resultMu.Lock()
	defer attempt.resultMu.Unlock()
	return attempt.result
}

func (attempt *ReceiverAttempt) Outcome() ReceiverAttemptOutcome {
	if attempt == nil {
		return newReceiverAttemptOutcome(
			protocolsession.OperationID{}, 0,
			receiverOperationDecision(
				ReceiverTerminalUnbound,
				ReceiverProvenanceSignalingAdapterContract,
			),
			ErrConfig,
			ErrConfig,
			nil,
			[]ReceiverCauseClass{ReceiverCauseConfiguration},
			false,
		)
	}
	attempt.resultMu.Lock()
	defer attempt.resultMu.Unlock()
	return attempt.outcome
}

func (attempt *ReceiverAttempt) Close() error {
	if attempt == nil {
		return nil
	}
	attempt.requestShutdown()
	<-attempt.done
	return attempt.Err()
}

type receiverEventKind uint8

const (
	receiverLocalCandidate receiverEventKind = iota + 1
	receiverControl
	receiverSignalingTerminated
	receiverChannelOpened
	receiverChannelDone
	receiverConnectionFailed
	receiverAttached
	receiverUnexpectedDataChannel
)

type receiverEvent struct {
	kind      receiverEventKind
	candidate v2signal.Candidate
	control   ReceiverControl
	terminal  ReceiverSignalingTermination
	lane      sessionruntime.LaneIdentity
	err       error
}

func (attempt *ReceiverAttempt) registerCallbacks() {
	attempt.peer.OnICECandidate(func(candidate *pion.ICECandidate) {
		if candidate == nil {
			return
		}
		value := candidate.ToJSON()
		attempt.push(receiverEvent{
			kind: receiverLocalCandidate,
			candidate: v2signal.Candidate{
				Binding: attempt.binding, Candidate: value.Candidate,
				SDPMid: value.SDPMid, SDPMLineIndex: value.SDPMLineIndex,
				UsernameFragment: value.UsernameFragment,
			},
		})
	})
	attempt.peer.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		if state == pion.PeerConnectionStateFailed {
			attempt.push(receiverEvent{
				kind: receiverConnectionFailed, err: errors.New("PeerConnection entered failed state"),
			})
		}
	})
	attempt.peer.OnDataChannel(func(channel *pion.DataChannel) {
		if channel != nil {
			_ = channel.Close()
		}
		attempt.push(receiverEvent{
			kind: receiverUnexpectedDataChannel,
			err:  errors.New("sender created an unauthorized peer DataChannel"),
		})
	})
}

func (attempt *ReceiverAttempt) push(event receiverEvent) {
	attempt.inboxMu.Lock()
	defer attempt.inboxMu.Unlock()
	if attempt.closed {
		return
	}
	select {
	case attempt.events <- event:
	default:
		attempt.cancel(ErrEventCapacity)
	}
}

func (attempt *ReceiverAttempt) run() {
	executionResult := attempt.execute()
	attempt.closeInbox()
	attempt.signalingMu.Lock()
	operation := attempt.operation
	attempt.signalingMu.Unlock()
	terminalDecision := receiverAttemptDecision{}
	terminalDiagnostics := error(nil)
	terminalDiagnosticsTruncated := false
	if operation != nil && executionResult.termination.ownedBy(operation.binding) {
		terminalDecision = executionResult.termination.decision
		terminalDiagnostics = executionResult.termination.diagnostics
		terminalDiagnosticsTruncated = executionResult.termination.diagnosticsTruncated
	} else if operation != nil {
		adapterFailure := receiverSignalingAdapterFailure(operation.binding, nil)
		terminalDecision = adapterFailure.decision
		terminalDiagnostics = adapterFailure.diagnostics
		terminalDiagnosticsTruncated = adapterFailure.diagnosticsTruncated
	}
	decision := mergeReceiverAttemptDecisions(executionResult.workflow.decision, terminalDecision)
	owner := decision.transitionOwner
	var operationID protocolsession.OperationID
	var localGeneration uint64
	if operation != nil {
		operationID = operation.operationID
		localGeneration = operation.localGeneration
	} else {
		localGeneration = executionResult.localGeneration
	}
	workflowPolicy := receiverCausePolicy{
		contextCanceled: executionResult.workflowContextCanceled,
	}
	terminalPolicy := receiverCausePolicy{}
	switch owner {
	case ReceiverTerminalLocal:
		terminalPolicy.contextCanceled = true
		terminalPolicy.operationMissing = ReceiverBenignLocalOperationMissing
	case ReceiverTerminalRemote:
		terminalPolicy.operationMissing = ReceiverBenignRemoteOperationMissing
	}
	workflowClassified := classifyReceiverCause(executionResult.workflow.cause, workflowPolicy)
	terminalClassified := classifyReceiverCause(terminalDiagnostics, terminalPolicy)
	classified := annotateReceiverDecisionDiagnostics(receiverCauseClassification{
		retained: joinReceiverResiduals([]error{
			workflowClassified.retained,
			terminalClassified.retained,
		}),
		benign: append(
			append([]ReceiverBenignCause(nil), workflowClassified.benign...),
			terminalClassified.benign...,
		),
		classes: append(
			append([]ReceiverCauseClass(nil), workflowClassified.classes...),
			terminalClassified.classes...,
		),
		complete: workflowClassified.complete && terminalClassified.complete,
	}, decision)
	if terminalDiagnosticsTruncated {
		classified.classes = append(classified.classes, ReceiverCauseUnknown)
	}
	classified.benign = uniqueReceiverBenignCauses(classified.benign)
	classified.classes = uniqueReceiverCauseClasses(classified.classes)
	diagnosticsTruncated := terminalDiagnosticsTruncated ||
		executionResult.workflow.diagnosticsTruncated || !classified.complete
	retainedCause := classified.retained
	cause := receiverClassifiedCause(classified)
	attempt.cancel(cause)
	outcome := newReceiverAttemptOutcome(
		operationID,
		localGeneration,
		decision,
		cause,
		retainedCause,
		classified.benign,
		classified.classes,
		diagnosticsTruncated,
	)
	attempt.resultMu.Lock()
	attempt.result = outcome.RetainedCause()
	attempt.outcome = outcome
	attempt.resultMu.Unlock()
	trace := ReceiverTerminationTrace{
		operationID: operationID, localGeneration: localGeneration,
		transitionOwner:       outcome.TransitionAuthority(),
		disposition:           outcome.Disposition(),
		transitionProvenance:  outcome.TransitionProvenance(),
		consequenceProvenance: outcome.ConsequenceProvenance(),
		diagnosticsTruncated:  outcome.DiagnosticsTruncated(),
		benignComponents:      outcome.BenignComponents(),
		retainedCauseClasses:  outcome.RetainedCauseClasses(),
		teardownTransitions:   executionResult.teardown.transitionSnapshot(),
		peerShutdownFailed:    executionResult.teardown.peerShutdownFailed(),
		channelDrainFailed:    executionResult.teardown.channelDrainFailed(),
	}
	close(attempt.done)
	attempt.emitTerminationTrace(trace)
}

func (attempt *ReceiverAttempt) emitTerminationTrace(trace ReceiverTerminationTrace) {
	// Observability is deliberately outside the completion barrier: a faulty
	// sink must not retain the exact signaling worker or make Close non-terminating.
	defer func() { _ = recover() }()
	attempt.factory.onTermination(trace)
}

func (attempt *ReceiverAttempt) closeInbox() {
	attempt.inboxMu.Lock()
	defer attempt.inboxMu.Unlock()
	if attempt.closed {
		return
	}
	attempt.closed = true
	for {
		select {
		case <-attempt.events:
		default:
			return
		}
	}
}
