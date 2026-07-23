// Package v2peer terminates authenticated suite-02 peer-signaling operations at
// the Go sender and owns the corresponding Pion PeerConnections.
package v2peer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

const (
	DefaultSTUNServer         = "stun:stun.l.google.com:19302"
	DefaultAttemptTimeout     = 10 * time.Second
	DefaultMaxCandidates      = v2signal.DefaultMaximumCandidates
	DefaultMaxActiveAttempts  = 4
	DefaultRetiredBindingTTL  = protocolsession.OperationTombstoneLifetime
	DefaultMaxRetiredBindings = 64

	maximumConfiguredCandidates = v2signal.MaximumCandidates
	maximumConfiguredAttempts   = 64
	maximumRetiredBindings      = 4_096
	handlerEventReserve         = 16
)

var (
	ErrConfig          = errors.New("v2 peer sender configuration is invalid")
	ErrProtocol        = errors.New("authenticated v2 peer signaling is invalid")
	ErrAttemptCapacity = errors.New("v2 peer attempt capacity is exhausted")
	ErrReplayCapacity  = errors.New("v2 peer replay tombstone capacity is exhausted")
	ErrEventCapacity   = errors.New("v2 peer signaling event capacity is exhausted")
	ErrNegotiation     = errors.New("v2 peer negotiation failed")
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
	Configuration      pion.Configuration
	PeerConnections    PeerConnectionFactory
	DataChannels       DataChannelAdapter
	AttemptTimeout     time.Duration
	MaxCandidates      int
	MaxActiveAttempts  int
	RetiredBindingTTL  time.Duration
	MaxRetiredBindings int
	Now                func() time.Time
	OnError            func(error)
}

type Factory struct {
	configuration      pion.Configuration
	peerConnections    PeerConnectionFactory
	dataChannels       DataChannelAdapter
	attemptTimeout     time.Duration
	maxCandidates      int
	maxActiveAttempts  int
	retiredBindingTTL  time.Duration
	maxRetiredBindings int
	now                func() time.Time
	onError            func(error)
}

func DefaultConfiguration() pion.Configuration {
	return pion.Configuration{ICEServers: []pion.ICEServer{{URLs: []string{DefaultSTUNServer}}}}
}

func NewFactory(config Config) (*Factory, error) {
	if config.AttemptTimeout < 0 || config.MaxCandidates < 0 || config.MaxActiveAttempts < 0 ||
		config.RetiredBindingTTL < 0 || config.MaxRetiredBindings < 0 {
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
	if config.MaxCandidates > maximumConfiguredCandidates ||
		config.MaxActiveAttempts > maximumConfiguredAttempts ||
		config.MaxRetiredBindings > maximumRetiredBindings ||
		config.MaxRetiredBindings < config.MaxActiveAttempts {
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
	if config.OnError == nil {
		config.OnError = func(error) {}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Factory{
		configuration: config.Configuration, peerConnections: config.PeerConnections,
		dataChannels: config.DataChannels, attemptTimeout: config.AttemptTimeout,
		maxCandidates: config.MaxCandidates, maxActiveAttempts: config.MaxActiveAttempts,
		retiredBindingTTL: config.RetiredBindingTTL, maxRetiredBindings: config.MaxRetiredBindings,
		now:     config.Now,
		onError: config.OnError,
	}, nil
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
		retiredOperations: make(map[peerOperation]retiredBinding),
		retiredBindings:   make(map[v2signal.Binding]retiredBinding),
	}, nil
}

func (factory *Factory) BeginOperationContinuation(
	requestKind protocolsession.MessageKind,
	canonicalRequestBody []byte,
) (protocolsession.OperationContinuationAuthority, bool, error) {
	if factory == nil {
		return nil, false, ErrConfig
	}
	return (v2signal.OperationContinuationClassifier{
		MaximumCandidates: factory.maxCandidates,
	}).BeginOperationContinuation(requestKind, canonicalRequestBody)
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
	kind      handlerEventKind
	ctx       context.Context
	operation peerOperation
	offer     v2signal.Offer
	candidate v2signal.Candidate
	rejection *peerOperationRejection
	completed chan error
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

	mu                 sync.Mutex
	attempts           map[peerOperation]*peerAttempt
	bindings           map[v2signal.Binding]peerOperation
	retiredOperations  map[peerOperation]retiredBinding
	retiredBindings    map[v2signal.Binding]retiredBinding
	replayBlockedUntil time.Time
	stopping           bool
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
	operation, ok := message.OperationID()
	if !ok || operation.IsZero() {
		return errors.Join(ErrProtocol, protocolsession.ErrInvalidOperationID)
	}
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, operation)
	if !ok || generation.IsZero() {
		return errors.Join(ErrProtocol, protocolsession.ErrUnknownOperation)
	}
	if !generation.IsActive() {
		return nil
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
	if err := handler.enqueue(ctx, event); err != nil {
		if !errors.Is(err, ErrEventCapacity) {
			return err
		}
		if event.kind == handlerOffer {
			handler.retireRejectedOffer(operationKey, event.offer.Binding)
		}
		handler.rejectOperation(ctx, operationKey, rejectionForEvent(event, err))
	}
	return nil
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
		if event.completed != nil {
			event.completed <- nil
		}
		return nil
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
			handler.retireRejectedOffer(event.operation, event.offer.Binding)
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
		handler.rejectOperation(eventContext, event.operation, rejected)
		return nil
	}
	if err != nil && ctx.Err() != nil {
		return err
	}
	if err != nil {
		handler.factory.onError(fmt.Errorf("cancel peer operation: %w", err))
	}
	return nil
}

func rejectionForEvent(event handlerEvent, cause error) *peerOperationRejection {
	if event.rejection != nil {
		return event.rejection
	}
	if event.kind == handlerCandidate {
		return &peerOperationRejection{
			code: protocolsession.PeerOperationCodeCandidates, message: peerCandidateFailureMessage, cause: cause,
		}
	}
	return &peerOperationRejection{
		code: protocolsession.PeerOperationCodeNegotiation, message: peerNegotiationFailureMessage, cause: cause,
	}
}

func (handler *senderHandler) rejectOperation(
	ctx context.Context,
	operation peerOperation,
	rejection *peerOperationRejection,
) {
	if rejection == nil {
		rejection = &peerOperationRejection{
			code:    protocolsession.PeerOperationCodeNegotiation,
			message: peerNegotiationFailureMessage,
			cause:   ErrNegotiation,
		}
	}
	handler.mu.Lock()
	attempt := handler.attempts[operation]
	handler.mu.Unlock()
	if attempt != nil {
		attempt.stop(rejection)
		return
	}
	failureContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureDeliveryTimeout)
	err := handler.session.FailPeerOperation(
		failureContext, operation.id, rejection.code, rejection.message,
	)
	cancel()
	handler.factory.onError(errors.Join(rejection, err))
}

var _ sessionruntime.SenderPeerHandlerFactory = (*Factory)(nil)
var _ sessionruntime.SenderPeerHandler = (*senderHandler)(nil)
