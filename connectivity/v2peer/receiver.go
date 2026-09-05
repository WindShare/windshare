package v2peer

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
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
	NewReceiverPeerConnection(context.Context, nativepeer.AttemptRequest) (ReceiverPeerConnection, error)
}

type ReceiverPeerConnectionFactoryFunc func(pion.Configuration) (ReceiverPeerConnection, error)

func (function ReceiverPeerConnectionFactoryFunc) NewReceiverPeerConnection(
	_ context.Context, request nativepeer.AttemptRequest,
) (ReceiverPeerConnection, error) {
	if function == nil {
		return nil, ErrConfig
	}
	return function(request.Configuration)
}

type ReceiverLaneSession interface {
	RequestLane(context.Context, uint32) (sessionruntime.LaneAttachmentGrant, error)
	AttachLane(
		context.Context,
		sessionruntime.LaneAttachmentGrant,
		protocolsession.FrameChannel,
		transfer.LaneRoute,
	) (sessionruntime.ReceiverLaneAdmissionResult, error)
}

const (
	DefaultReceiverTerminationObservationCapacity = 64
	maximumReceiverTerminationObservationCapacity = 4_096
)

type ReceiverFactoryConfig struct {
	Native                                 *nativepeer.NativePeerConnectivity
	Configuration                          pion.Configuration
	PeerConnections                        ReceiverPeerConnectionFactory
	DataChannels                           DataChannelAdapter
	NegotiationBudget                      time.Duration
	AdmissionBudget                        time.Duration
	PhaseTimers                            PeerPhaseTimerSource
	MaxCandidates                          int
	Random                                 io.Reader
	ReceiverTerminationObservationCapacity int
	PeerDiagnosticObservationCapacity      int
}

type ReceiverFactory struct {
	native                *nativepeer.NativePeerConnectivity
	configuration         pion.Configuration
	peerConnections       ReceiverPeerConnectionFactory
	dataChannels          DataChannelAdapter
	negotiationBudget     time.Duration
	admissionBudget       time.Duration
	phaseTimers           PeerPhaseTimerSource
	maxCandidates         int
	random                io.Reader
	terminations          *observationSource[ReceiverTerminationTrace]
	diagnostics           *peerDiagnosticReporter
	readMu                sync.Mutex
	observationMu         sync.Mutex
	observationsCompleted bool
	observationCompletion ReceiverObservationCompletion
}

func NewReceiverFactory(config ReceiverFactoryConfig) (*ReceiverFactory, error) {
	if config.NegotiationBudget < 0 || config.AdmissionBudget < 0 || config.MaxCandidates < 0 ||
		config.MaxCandidates > maximumConfiguredCandidates ||
		config.ReceiverTerminationObservationCapacity < 0 ||
		config.ReceiverTerminationObservationCapacity > maximumReceiverTerminationObservationCapacity ||
		config.PeerDiagnosticObservationCapacity < 0 ||
		config.PeerDiagnosticObservationCapacity > maximumPeerDiagnosticObservationCapacity {
		return nil, ErrConfig
	}
	if config.NegotiationBudget == 0 {
		config.NegotiationBudget = DefaultPeerNegotiationBudget
	}
	if config.AdmissionBudget == 0 {
		config.AdmissionBudget = DefaultPeerAdmissionBudget
	}
	if !validPeerPhaseBudgets(config.NegotiationBudget, config.AdmissionBudget) {
		return nil, ErrConfig
	}
	if config.MaxCandidates == 0 {
		config.MaxCandidates = DefaultMaxCandidates
	}
	if config.PhaseTimers == nil {
		config.PhaseTimers = systemPeerPhaseTimerSource{}
	}
	if config.Native == nil {
		config.Native = nativepeer.New(nativepeer.Config{})
	}
	if config.DataChannels == nil {
		config.DataChannels = DataChannelAdapterFunc(func(channel *pion.DataChannel) (PeerDataChannel, error) {
			return transportwebrtc.NewChannel(channel)
		})
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	terminations, err := newObservationSource[ReceiverTerminationTrace](
		config.ReceiverTerminationObservationCapacity,
	)
	if err != nil {
		return nil, errors.Join(ErrConfig, err)
	}
	diagnostics, err := newPeerDiagnosticReporter(config.PeerDiagnosticObservationCapacity)
	if err != nil {
		return nil, errors.Join(ErrConfig, err)
	}
	factory := &ReceiverFactory{
		native:        config.Native,
		configuration: config.Configuration, peerConnections: config.PeerConnections,
		dataChannels:      config.DataChannels,
		negotiationBudget: config.NegotiationBudget, admissionBudget: config.AdmissionBudget,
		phaseTimers:   config.PhaseTimers,
		maxCandidates: config.MaxCandidates, random: config.Random,
		terminations: terminations, diagnostics: diagnostics,
	}
	return factory, nil
}

func (factory *ReceiverFactory) ReceiverTerminationObservations() <-chan ReceiverTerminationTrace {
	if factory == nil {
		return nil
	}
	return factory.terminations.observations()
}

func (factory *ReceiverFactory) PeerDiagnostics() <-chan PeerDiagnosticObservation {
	if factory == nil {
		return nil
	}
	return factory.diagnostics.observations()
}

// CompleteObservations cuts termination admission before publishing its final
// capacity fact and cutting diagnostics. Attempts must be joined by the caller
// first so the returned producer snapshot is final.
func (factory *ReceiverFactory) CompleteObservations() ReceiverObservationCompletion {
	if factory == nil {
		return ReceiverObservationCompletion{}
	}
	factory.observationMu.Lock()
	defer factory.observationMu.Unlock()
	if factory.observationsCompleted {
		return factory.observationCompletion
	}
	terminations := factory.terminations.completeObservations()
	factory.diagnostics.reportCount(
		PeerDiagnosticReceiverTermination,
		PeerDiagnosticStreamCapacity,
		terminations.Loss.CapacityDropped,
	)
	factory.observationCompletion = ReceiverObservationCompletion{
		Terminations: terminations,
		Diagnostics:  factory.diagnostics.completeObservations(),
	}
	factory.observationsCompleted = true
	return factory.observationCompletion
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
	phases            *peerPhaseLifecycle
	phaseContext      context.Context
	transport         *ownedPeerDataChannel
}

func (factory *ReceiverFactory) Start(
	parent context.Context,
	signaling ReceiverSignaling,
	lanes ReceiverLaneSession,
) (*ReceiverAttempt, error) {
	if factory == nil {
		return nil, ErrConfig
	}
	binding, err := factory.newBinding()
	if err != nil {
		return nil, err
	}
	return factory.StartBinding(parent, signaling, lanes, binding)
}

// StartPreparedBinding transfers an admitted preparation into one timed attempt.
func (factory *ReceiverFactory) StartPreparedBinding(parent context.Context, signaling ReceiverSignaling, lanes ReceiverLaneSession, binding v2signal.Binding, prepared *nativepeer.PreparedAttempt) (*ReceiverAttempt, error) {
	if factory == nil || signaling == nil || lanes == nil || parent == nil || binding.Validate() != nil {
		return nil, ErrConfig
	}
	ctx, cancel := context.WithCancelCause(parent)
	phases := newPeerPhaseLifecycle(
		factory.phaseTimers,
		factory.negotiationBudget,
		factory.admissionBudget,
	)
	phases.staged = factory.peerConnections == nil
	phaseContext, err := phases.beginNegotiation(ctx)
	if err != nil {
		cancel(err)
		return nil, err
	}
	fail := func(cause error, peer peerCloseOwner, channel peerCloseOwner) (*ReceiverAttempt, error) {
		phases.terminate(cause)
		cancel(cause)
		teardown := teardownPeerTransport(peer, channel)
		return nil, errors.Join(cause, teardown.cause())
	}
	var sessionID protocolsession.ProtocolSessionID
	if session, ok := lanes.(interface {
		ProtocolSessionID() protocolsession.ProtocolSessionID
	}); ok {
		sessionID = session.ProtocolSessionID()
	}
	request := nativepeer.AttemptRequest{Configuration: factory.configuration, ProtocolSessionID: [16]byte(sessionID), Binding: binding}
	var peer ReceiverPeerConnection
	if factory.peerConnections != nil {
		peer, err = factory.peerConnections.NewReceiverPeerConnection(phaseContext, request)
	} else {
		if !prepared.Matches(request.ProtocolSessionID, binding) {
			return fail(ErrConfig, nil, nil)
		}
		connection, startErr := prepared.Start(phaseContext)
		err = startErr
		if connection != nil {
			peer = connection
		}
	}
	if err != nil || peer == nil {
		return fail(errors.Join(ErrNegotiation, err), peer, nil)
	}
	raw, err := peer.CreateDataChannel(
		transportwebrtc.ChannelLabel,
		transportwebrtc.DefaultDataChannelInit(),
	)
	if err != nil || raw == nil {
		return fail(errors.Join(ErrNegotiation, err), peer, nil)
	}
	channel, err := factory.dataChannels.WrapDataChannel(raw)
	if err != nil || channel == nil {
		return fail(errors.Join(errChannelAdmission, err), peer, raw)
	}
	transport := newOwnedPeerDataChannel(peer, channel)
	attempt := &ReceiverAttempt{
		factory: factory, signaling: signaling, lanes: lanes, peer: peer,
		channel: transport, transport: transport, binding: binding,
		events: make(chan receiverEvent, factory.maxCandidates*2+attemptEventReserve),
		ctx:    ctx, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}),
		phases: phases, phaseContext: phaseContext,
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
	return v2signal.Binding{PeerPathID: pathID, AttemptID: attemptID, AttemptSequence: 1}, nil
}

func readReceiverSignalID[T ~[v2signal.IdentityBytes]byte](source io.Reader) (T, error) {
	for range 4 {
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
	grant     sessionruntime.LaneAttachmentGrant
	admission sessionruntime.ReceiverLaneAdmissionResult
	err       error
}

func (attempt *ReceiverAttempt) registerCallbacks() {
	observePeerICE(attempt.peer, attempt.phases)
	accept := candidateAdmission(attempt.peer, attempt.factory.maxCandidates)
	attempt.peer.OnICECandidate(func(candidate *pion.ICECandidate) {
		if !accept(candidate) {
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
	attempt.emitTerminationTrace(trace)
	// Close/Done are producer quiescence boundaries: publication admission must
	// happen first so factory completion cannot overtake the terminal fact.
	close(attempt.done)
}

func (attempt *ReceiverAttempt) emitTerminationTrace(trace ReceiverTerminationTrace) {
	if attempt == nil || attempt.factory == nil || attempt.factory.terminations == nil {
		return
	}
	attempt.factory.terminations.publish(cloneReceiverTerminationTrace(trace))
}

func cloneReceiverTerminationTrace(trace ReceiverTerminationTrace) ReceiverTerminationTrace {
	clone := trace
	clone.benignComponents = append([]ReceiverBenignCause(nil), trace.benignComponents...)
	clone.retainedCauseClasses = append([]ReceiverCauseClass(nil), trace.retainedCauseClasses...)
	clone.teardownTransitions = append([]PeerTeardownTransition(nil), trace.teardownTransitions...)
	return clone
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

func (factory *ReceiverFactory) NativeConnectivity() *nativepeer.NativePeerConnectivity {
	if factory.peerConnections != nil {
		return nil
	}
	return factory.native
}
