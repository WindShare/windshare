// Package v2peer terminates authenticated suite-02 peer-signaling operations at
// the Go sender and owns the corresponding Pion PeerConnections.
package v2peer

import (
	"context"
	"errors"
	"sync"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

const (
	DefaultSTUNServer                            = "stun:stun.l.google.com:19302"
	DefaultAttemptTimeout                        = 10 * time.Second
	DefaultMaxCandidates                         = v2signal.DefaultMaximumCandidates
	DefaultMaxActiveAttempts                     = 4
	DefaultRetiredBindingTTL                     = protocolsession.OperationTombstoneLifetime
	DefaultMaxRetiredBindings                    = 64
	DefaultSenderObservationCapacity             = 256
	defaultEvidenceReplacementWavesPerActiveSlot = 64
	// A session normally needs one direct attempt. Sixty-four replacement waves
	// per active slot leave broad diagnostic headroom without letting an
	// authenticated peer turn exact lifetime claims into unbounded state.
	DefaultMaxSessionEvidenceIdentities = DefaultMaxActiveAttempts * defaultEvidenceReplacementWavesPerActiveSlot

	maximumConfiguredCandidates        = v2signal.MaximumCandidates
	maximumConfiguredAttempts          = 64
	maximumRetiredBindings             = 4_096
	maximumSessionEvidenceIdentities   = 65_536
	maximumSenderObservationCapacity   = 4_096
	handlerEventReserve                = 16
	rejectedOfferAuthoritySDP          = "v=0\r\n"
	peerEvidenceCapacityFailureMessage = "Sender peer evidence identity capacity exhausted"
)

var (
	ErrConfig                   = errors.New("v2 peer sender configuration is invalid")
	ErrProtocol                 = errors.New("authenticated v2 peer signaling is invalid")
	ErrAttemptCapacity          = errors.New("v2 peer attempt capacity is exhausted")
	ErrReplayCapacity           = errors.New("v2 peer replay tombstone capacity is exhausted")
	ErrEventCapacity            = errors.New("v2 peer signaling event capacity is exhausted")
	ErrEvidenceIdentityCapacity = errors.New("v2 peer session evidence identity capacity is exhausted")
	ErrNegotiation              = errors.New("v2 peer negotiation failed")
)

type PeerConnection interface {
	OnICECandidate(func(*pion.ICECandidate))
	OnConnectionStateChange(func(pion.PeerConnectionState))
	OnDataChannel(func(*pion.DataChannel))
	SetRemoteDescription(pion.SessionDescription) error
	CreateAnswer(*pion.AnswerOptions) (pion.SessionDescription, error)
	SetLocalDescription(pion.SessionDescription) error
	LocalDescription() *pion.SessionDescription
	AddICECandidate(pion.ICECandidateInit) error
	Close() error
}

type PeerConnectionFactory interface {
	NewPeerConnection(pion.Configuration) (PeerConnection, error)
}

type PeerConnectionFactoryFunc func(pion.Configuration) (PeerConnection, error)

func (function PeerConnectionFactoryFunc) NewPeerConnection(
	configuration pion.Configuration,
) (PeerConnection, error) {
	if function == nil {
		return nil, ErrConfig
	}
	return function(configuration)
}

type PeerDataChannel interface {
	protocolsession.FrameChannel
	Opened() <-chan struct{}
	Done() <-chan struct{}
	Err() error
}

type DataChannelAdapter interface {
	WrapDataChannel(*pion.DataChannel) (PeerDataChannel, error)
}

type DataChannelAdapterFunc func(*pion.DataChannel) (PeerDataChannel, error)

func (function DataChannelAdapterFunc) WrapDataChannel(
	channel *pion.DataChannel,
) (PeerDataChannel, error) {
	if function == nil {
		return nil, ErrConfig
	}
	return function(channel)
}

type Config struct {
	Configuration                pion.Configuration
	PeerConnections              PeerConnectionFactory
	DataChannels                 DataChannelAdapter
	Observer                     SenderAttemptObserver
	AttemptTimeout               time.Duration
	MaxCandidates                int
	MaxActiveAttempts            int
	RetiredBindingTTL            time.Duration
	MaxRetiredBindings           int
	MaxSessionEvidenceIdentities int
	SenderObservationCapacity    int
	Now                          func() time.Time
	DiagnosticObserver           PeerDiagnosticObserver
}

type Factory struct {
	configuration                pion.Configuration
	peerConnections              PeerConnectionFactory
	dataChannels                 DataChannelAdapter
	attemptTimeout               time.Duration
	maxCandidates                int
	maxActiveAttempts            int
	retiredBindingTTL            time.Duration
	maxRetiredBindings           int
	maxSessionEvidenceIdentities int
	now                          func() time.Time
	diagnostics                  *peerDiagnosticReporter
	senderObservations           *observationDispatcher[SenderAttemptObservation]
}

func DefaultConfiguration() pion.Configuration {
	return pion.Configuration{ICEServers: []pion.ICEServer{{URLs: []string{DefaultSTUNServer}}}}
}

func NewFactory(config Config) (*Factory, error) {
	if config.AttemptTimeout < 0 || config.MaxCandidates < 0 || config.MaxActiveAttempts < 0 ||
		config.RetiredBindingTTL < 0 || config.MaxRetiredBindings < 0 ||
		config.MaxSessionEvidenceIdentities < 0 || config.SenderObservationCapacity < 0 {
		return nil, ErrConfig
	}
	if config.AttemptTimeout == 0 {
		config.AttemptTimeout = DefaultAttemptTimeout
	}
	if config.MaxCandidates == 0 {
		config.MaxCandidates = DefaultMaxCandidates
	}
	if config.MaxActiveAttempts == 0 {
		config.MaxActiveAttempts = DefaultMaxActiveAttempts
	}
	if config.RetiredBindingTTL == 0 {
		config.RetiredBindingTTL = DefaultRetiredBindingTTL
	}
	if config.MaxRetiredBindings == 0 {
		config.MaxRetiredBindings = DefaultMaxRetiredBindings
	}
	if config.MaxSessionEvidenceIdentities == 0 {
		config.MaxSessionEvidenceIdentities = DefaultMaxSessionEvidenceIdentities
	}
	if config.Observer != nil && config.SenderObservationCapacity == 0 {
		config.SenderObservationCapacity = DefaultSenderObservationCapacity
	}
	if config.MaxCandidates > maximumConfiguredCandidates ||
		config.MaxActiveAttempts > maximumConfiguredAttempts ||
		config.MaxRetiredBindings > maximumRetiredBindings ||
		config.MaxRetiredBindings < config.MaxActiveAttempts ||
		config.MaxSessionEvidenceIdentities > maximumSessionEvidenceIdentities ||
		config.MaxSessionEvidenceIdentities < config.MaxActiveAttempts ||
		config.SenderObservationCapacity > maximumSenderObservationCapacity {
		return nil, ErrConfig
	}
	if config.PeerConnections == nil {
		config.PeerConnections = PeerConnectionFactoryFunc(func(configuration pion.Configuration) (PeerConnection, error) {
			return pion.NewPeerConnection(configuration)
		})
	}
	if config.DataChannels == nil {
		config.DataChannels = DataChannelAdapterFunc(func(channel *pion.DataChannel) (PeerDataChannel, error) {
			return transportwebrtc.NewChannel(channel)
		})
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	factory := &Factory{
		configuration: config.Configuration, peerConnections: config.PeerConnections,
		dataChannels: config.DataChannels, attemptTimeout: config.AttemptTimeout,
		maxCandidates: config.MaxCandidates, maxActiveAttempts: config.MaxActiveAttempts,
		retiredBindingTTL: config.RetiredBindingTTL, maxRetiredBindings: config.MaxRetiredBindings,
		maxSessionEvidenceIdentities: config.MaxSessionEvidenceIdentities,
		now:                          config.Now,
		diagnostics:                  newPeerDiagnosticReporter(config.DiagnosticObserver),
	}
	if config.Observer != nil {
		factory.senderObservations = newObservationDispatcher(
			config.SenderObservationCapacity,
			config.Observer.ObserveSenderAttempt,
			func() { factory.reportDiagnostic(PeerDiagnosticSenderAttempt, PeerDiagnosticObserverPanic) },
			func() { factory.reportDiagnostic(PeerDiagnosticSenderAttempt, PeerDiagnosticObserverCapacity) },
		)
		if contextual, ok := config.Observer.(SenderAttemptContextObserver); ok {
			factory.senderObservations.observeContext = contextual.ObserveSenderAttemptContext
		}
	}
	return factory, nil
}

func (factory *Factory) NewSenderPeerHandler(
	session sessionruntime.SenderPeerSession,
) (sessionruntime.SenderPeerHandler, error) {
	if factory == nil || session == nil || session.ShareInstance().IsZero() || session.ProtocolSessionID().IsZero() {
		return nil, ErrConfig
	}
	capacity := factory.maxActiveAttempts*(factory.maxCandidates+3) + handlerEventReserve
	return &senderHandler{
		factory: factory, session: session, events: make(chan handlerEvent, capacity),
		attempts:          make(map[peerOperation]*peerAttempt),
		bindings:          make(map[v2signal.Binding]peerOperation),
		evidenceAuthority: newSenderEvidenceAuthority(factory.maxSessionEvidenceIdentities),
		retiredOperations: make(map[peerOperation]retiredBinding),
		retiredBindings:   make(map[v2signal.Binding]retiredBinding),
	}, nil
}

// CompleteObservations closes sender-attempt callback admission before draining
// diagnostics, because attempt loss may itself publish a cumulative diagnostic.
func (factory *Factory) CompleteObservations(ctx context.Context) SenderObservationCompletion {
	if factory == nil {
		return SenderObservationCompletion{
			Attempts:    ObservationCompletion{Drained: true},
			Diagnostics: ObservationCompletion{Drained: true},
		}
	}
	return SenderObservationCompletion{
		Attempts:    factory.senderObservations.complete(ctx),
		Diagnostics: factory.diagnostics.complete(ctx),
	}
}

func (factory *Factory) BeginOperationContinuation(
	requestKind protocolsession.MessageKind,
	canonicalRequestBody []byte,
) (protocolsession.OperationContinuationAuthority, bool, error) {
	if factory == nil {
		return nil, false, ErrConfig
	}
	classifier := v2signal.OperationContinuationClassifier{
		MaximumCandidates: factory.maxCandidates,
	}
	authority, tracked, err := classifier.BeginOperationContinuation(requestKind, canonicalRequestBody)
	if err == nil || requestKind != protocolsession.MessagePeerOffer {
		return authority, tracked, err
	}
	binding, recovered := recoverOfferBinding(canonicalRequestBody)
	if !recovered {
		return authority, tracked, err
	}
	// OperationTable must admit the generation before the sender handler can
	// publish its rejection. Rebuilding only the continuation authority from a
	// canonical placeholder preserves v2signal's single binding/scope owner while
	// leaving the malformed original body for the handler to reject and observe.
	placeholder, encodeErr := v2signal.EncodeOffer(v2signal.Offer{
		Binding: binding,
		SDP:     rejectedOfferAuthoritySDP,
	})
	if encodeErr != nil {
		return nil, true, errors.Join(err, encodeErr)
	}
	return classifier.BeginOperationContinuation(requestKind, placeholder)
}

func (factory *Factory) ClassifyUnboundOperationContinuation(
	kind protocolsession.MessageKind,
	canonicalBody []byte,
) (protocolsession.OperationContinuationScope, bool, error) {
	if factory == nil {
		return protocolsession.OperationContinuationScope{}, false, ErrConfig
	}
	return (v2signal.OperationContinuationClassifier{
		MaximumCandidates: factory.maxCandidates,
	}).ClassifyUnboundOperationContinuation(kind, canonicalBody)
}

type handlerEventKind uint8

const (
	handlerOffer handlerEventKind = iota + 1
	handlerCandidate
	handlerReject
	handlerCancel
)

type handlerEvent struct {
	kind         handlerEventKind
	ctx          context.Context
	operation    peerOperation
	offer        v2signal.Offer
	candidate    v2signal.Candidate
	offerBinding *v2signal.Binding
	rejection    *peerOperationRejection
	completed    chan error
}

type peerOperation struct {
	id         protocolsession.OperationID
	generation protocolsession.OperationGeneration
}

type senderHandler struct {
	factory *Factory
	session sessionruntime.SenderPeerSession
	events  chan handlerEvent
	inboxMu sync.Mutex
	closed  bool
	ingress sync.WaitGroup

	mu       sync.Mutex
	attempts map[peerOperation]*peerAttempt
	bindings map[v2signal.Binding]peerOperation
	// Evidence identity is scoped to the ProtocolSession, while replay
	// tombstones intentionally expire. Keeping these authorities separate is
	// what makes one (session,path,attempt,side) stream terminal exactly once.
	evidenceAuthority  senderEvidenceAuthority
	retiredOperations  map[peerOperation]retiredBinding
	retiredBindings    map[v2signal.Binding]retiredBinding
	replayBlockedUntil time.Time
	stopping           bool
	runtimeContext     context.Context
	work               sync.WaitGroup
}

type retiredBinding struct {
	operation peerOperation
	binding   v2signal.Binding
	expiresAt time.Time
}

func (handler *senderHandler) HandleMessage(
	ctx context.Context,
	message protocolsession.Message,
) error {
	if !handler.beginIngress() {
		return context.Canceled
	}
	defer handler.ingress.Done()
	return handler.handleMessage(ctx, message)
}

func (handler *senderHandler) handleMessage(
	ctx context.Context,
	message protocolsession.Message,
) error {
	operation, ok := message.OperationID()
	if !ok || operation.IsZero() {
		return errors.Join(ErrProtocol, protocolsession.ErrInvalidOperationID)
	}
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, operation)
	if !ok || generation.IsZero() {
		return errors.Join(ErrProtocol, protocolsession.ErrUnknownOperation)
	}
	operationKey := peerOperation{id: operation, generation: generation}
	event := handlerEvent{ctx: ctx, operation: operationKey}
	var err error
	switch message.Kind() {
	case protocolsession.MessagePeerOffer:
		event.kind = handlerOffer
		event.offer, err = v2signal.DecodeOffer(message.Body())
		if err != nil {
			event.kind = handlerReject
			if binding, recovered := recoverOfferBinding(message.Body()); recovered {
				event.offerBinding = &binding
			}
			event.rejection = &peerOperationRejection{
				code: protocolsession.PeerOperationCodeNegotiation, message: peerNegotiationFailureMessage, cause: err,
			}
		}
	case protocolsession.MessagePeerCandidate:
		event.kind = handlerCandidate
		event.candidate, err = v2signal.DecodeCandidate(message.Body())
		if err != nil {
			event.kind = handlerReject
			event.rejection = &peerOperationRejection{
				code: protocolsession.PeerOperationCodeCandidates, message: peerCandidateFailureMessage, cause: err,
			}
		}
	default:
		return errors.Join(ErrProtocol, protocolsession.ErrUnknownMessageKind)
	}
	if !generation.IsActive() {
		return handler.terminalizeUnstartedOffer(event, handler.unstartedOfferFailure())
	}
	if err := handler.enqueue(ctx, event); err != nil {
		if errors.Is(err, ErrEventCapacity) {
			return handler.rejectOperation(
				ctx, operationKey, rejectionForEvent(event, err), offerBindingForEvent(event),
			)
		}
		return errors.Join(err, handler.terminalizeUnstartedOffer(event, handler.unstartedOfferFailure()))
	}
	return nil
}

func (handler *senderHandler) beginIngress() bool {
	handler.inboxMu.Lock()
	defer handler.inboxMu.Unlock()
	if handler.closed {
		return false
	}
	// Admission and shutdown share inboxMu so Wait can never race a new Add.
	// Once admitted, ingress owns its evidence claim until HandleMessage returns.
	handler.ingress.Add(1)
	return true
}

func (handler *senderHandler) Cancel(
	ctx context.Context,
	operation protocolsession.OperationID,
) error {
	if operation.IsZero() {
		return errors.Join(ErrProtocol, protocolsession.ErrInvalidOperationID)
	}
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, operation)
	if !ok || generation.IsZero() {
		return errors.Join(ErrProtocol, protocolsession.ErrUnknownOperation)
	}
	if !generation.IsCurrent() {
		return nil
	}
	completed := make(chan error, 1)
	if err := handler.enqueueCancellation(ctx, handlerEvent{
		kind: handlerCancel, ctx: ctx,
		operation: peerOperation{id: operation, generation: generation}, completed: completed,
	}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-completed:
		return err
	}
}

func (handler *senderHandler) enqueueCancellation(ctx context.Context, event handlerEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	handler.inboxMu.Lock()
	defer handler.inboxMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if handler.closed {
		return context.Canceled
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case handler.events <- event:
		return nil
	default:
		return ErrEventCapacity
	}
}

func (handler *senderHandler) enqueue(ctx context.Context, event handlerEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	handler.inboxMu.Lock()
	defer handler.inboxMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if handler.closed {
		return context.Canceled
	}
	select {
	case handler.events <- event:
		return nil
	default:
		return ErrEventCapacity
	}
}

func (handler *senderHandler) Run(ctx context.Context) error {
	handler.mu.Lock()
	handler.runtimeContext = ctx
	handler.mu.Unlock()
	defer handler.stopAll()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-handler.events:
			if err := handler.handleRunEvent(ctx, event); err != nil {
				return err
			}
		}
	}
}

func (handler *senderHandler) handleRunEvent(ctx context.Context, event handlerEvent) error {
	eventContext := protocolsession.RetainMessageContext(ctx, event.ctx)
	if event.kind != handlerCancel && !event.operation.generation.IsZero() &&
		!event.operation.generation.IsActive() {
		terminalErr := handler.terminalizeUnstartedOffer(event, handler.unstartedOfferFailure())
		if event.completed != nil {
			event.completed <- nil
		}
		return terminalErr
	}
	var err error
	var rejected *peerOperationRejection
	switch event.kind {
	case handlerOffer:
		err = handler.startAttempt(eventContext, event.operation, event.offer)
		if err != nil {
			rejected = &peerOperationRejection{
				code:    protocolsession.PeerOperationCodeNegotiation,
				message: peerNegotiationFailureMessage,
				cause:   err,
			}
		}
	case handlerCandidate:
		err = handler.acceptCandidate(event.operation, event.candidate)
		if err != nil {
			rejected = &peerOperationRejection{
				code:    protocolsession.PeerOperationCodeCandidates,
				message: peerCandidateFailureMessage,
				cause:   err,
			}
		}
	case handlerReject:
		rejected = event.rejection
	case handlerCancel:
		err = handler.cancelAttempt(eventContext, event.operation)
	default:
		return ErrProtocol
	}
	if event.completed != nil {
		event.completed <- err
	}
	if rejected != nil {
		return handler.rejectOperation(
			eventContext, event.operation, rejected, offerBindingForEvent(event),
		)
	}
	if err != nil && ctx.Err() != nil {
		return err
	}
	if err != nil {
		handler.factory.reportDiagnostic(PeerDiagnosticSenderAttempt, PeerDiagnosticCleanupResidue)
	}
	return nil
}

var _ sessionruntime.SenderPeerHandlerFactory = (*Factory)(nil)
var _ sessionruntime.SenderPeerHandler = (*senderHandler)(nil)
