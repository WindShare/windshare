package protocolsession

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	RouterControlQueueLimit   = 256
	RouterDataQueueLimit      = 1024
	RouterMaximumControlBurst = 8
)

var (
	ErrNilRuntimeDependency = errors.New("protocolsession: runtime dependency must not be nil")
	ErrRouterControlFull    = errors.New("protocolsession: inbound control queue is full")
	ErrRouterDataFull       = errors.New("protocolsession: inbound data queue is full")
	ErrHandlerRegistered    = errors.New("protocolsession: message kind already has a handler")
	ErrHandlerMissing       = errors.New("protocolsession: message kind has no handler")
	ErrInvalidRouteEvent    = errors.New("protocolsession: route event is invalid")
	ErrRouterConsumerBusy   = errors.New("protocolsession: router Next already has a consumer")
)

// RouterLimits permits lower local admission limits without changing wire
// limits. The default mirrors the writer's frame-count isolation.
type RouterLimits struct {
	ControlFrames int
	DataFrames    int
}

var DefaultRouterLimits = RouterLimits{
	ControlFrames: RouterControlQueueLimit,
	DataFrames:    RouterDataQueueLimit,
}

// RouteEvent is immutable. An overflow event has ErrRouterDataFull and only an
// operation identity; a normal event has a Message and nil Error.
type RouteEvent struct {
	message        Message
	hasMessage     bool
	operationID    OperationID
	generation     OperationGeneration
	err            error
	tracksData     bool
	messageContext context.Context
}

func (event RouteEvent) Message() (Message, bool) { return event.message, event.hasMessage }
func (event RouteEvent) OperationID() OperationID { return event.operationID }
func (event RouteEvent) Error() error             { return event.err }

// OutboundMessagePolicy is the narrow lifecycle contract consumed by
// SessionWriter. It lets a later facade atomically compose routing and writing
// without exposing FrameChannel to business services.
type OutboundMessagePolicy interface {
	AdmitOutbound(message Message, permit OutboundOperationPermit) (OutboundAdmission, error)
	AcceptOutboundReplay(message Message, permit OutboundReplayPermit) (OutboundAdmission, error)
	AcceptOutboundTerminal() error
	OutboundDirection() Direction
}

type outboundOperationPermitContextKey struct{}
type operationGenerationContextKey struct{}

func WithOperationGeneration(ctx context.Context, generation OperationGeneration) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, operationGenerationContextKey{}, generation)
}

func OperationGenerationFromContext(
	ctx context.Context,
	operationID OperationID,
) (OperationGeneration, bool) {
	if ctx == nil || operationID.IsZero() {
		return OperationGeneration{}, false
	}
	generation, ok := ctx.Value(operationGenerationContextKey{}).(OperationGeneration)
	return generation, ok && !generation.IsZero() && generation.operationID == operationID
}

// WithOutboundOperationPermit propagates an opaque capability through an async
// handler boundary. Callers can carry but cannot manufacture its private
// operation-generation identity.
func WithOutboundOperationPermit(
	ctx context.Context,
	permit OutboundOperationPermit,
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, outboundOperationPermitContextKey{}, permit)
}

func OutboundOperationPermitFromContext(
	ctx context.Context,
	operationID OperationID,
) (OutboundOperationPermit, bool) {
	if ctx == nil || operationID.IsZero() {
		return OutboundOperationPermit{}, false
	}
	permit, ok := ctx.Value(outboundOperationPermitContextKey{}).(OutboundOperationPermit)
	return permit, ok && permit.operationID == operationID && !permit.IsZero()
}

type InboundMessageRouter interface {
	RouteInbound(ctx context.Context, message Message) (OperationDisposition, error)
	RouteAuthenticatedOperationViolation(
		context.Context,
		Message,
		AuthenticatedOperationViolation,
	) (bool, error)
	TerminateLocal() error
	InboundDirection() Direction
}

// MessageHandler is deliberately non-transport-facing. A handler should admit
// work into its own bounded service queue and return promptly; Dispatch is kept
// outside the sole Recv pump so slow business code cannot capture lane input.
type MessageHandler interface {
	HandleMessage(context.Context, Message) error
}

type MessageHandlerFunc func(context.Context, Message) error

func (f MessageHandlerFunc) HandleMessage(ctx context.Context, message Message) error {
	return f(ctx, message)
}

type retainedMessageContext struct {
	context.Context
	values context.Context
}

func (ctx retainedMessageContext) Value(key any) any {
	if ctx.values != nil {
		if value := ctx.values.Value(key); value != nil {
			return value
		}
	}
	return ctx.Context.Value(key)
}

// RetainMessageContext lets asynchronous handlers inherit authenticated values
// from ingress while their cancellation remains owned by the session/service
// lifetime. A physical lane ending must not cancel work that can migrate.
func RetainMessageContext(lifetime context.Context, messageContext context.Context) context.Context {
	if lifetime == nil {
		lifetime = context.Background()
	}
	if messageContext == nil {
		return lifetime
	}
	return retainedMessageContext{Context: lifetime, values: messageContext}
}

// RoleRouter owns bounded dispatch queues. RouteInbound never invokes business
// code, so even a stopped data consumer cannot capture the sole Recv pump.
type RoleRouter struct {
	role       Role
	operations *OperationTable
	control    chan RouteEvent
	data       chan RouteEvent
	done       chan struct{}

	handlerMu sync.RWMutex
	handlers  map[MessageKind]MessageHandler

	queueMu     sync.Mutex
	pendingData map[OperationGeneration]uint32
	ingressMu   sync.Mutex
	lifecycleMu sync.RWMutex
	closed      bool

	nextActive   atomic.Bool
	controlBurst int
}

func NewRoleRouter(role Role, operations *OperationTable) (*RoleRouter, error) {
	return NewRoleRouterWithLimits(role, operations, DefaultRouterLimits)
}

func NewRoleRouterWithLimits(
	role Role,
	operations *OperationTable,
	limits RouterLimits,
) (*RoleRouter, error) {
	if _, err := role.InboundDirection(); err != nil {
		return nil, err
	}
	if operations == nil {
		return nil, fmt.Errorf("%w: operation table", ErrNilRuntimeDependency)
	}
	if limits.ControlFrames <= 0 || limits.ControlFrames > RouterControlQueueLimit ||
		limits.DataFrames <= 0 || limits.DataFrames > RouterDataQueueLimit {
		return nil, fmt.Errorf("protocolsession: invalid router limits: %+v", limits)
	}
	return &RoleRouter{
		role:        role,
		operations:  operations,
		control:     make(chan RouteEvent, limits.ControlFrames),
		data:        make(chan RouteEvent, limits.DataFrames),
		done:        make(chan struct{}),
		handlers:    make(map[MessageKind]MessageHandler),
		pendingData: make(map[OperationGeneration]uint32),
	}, nil
}

// Next gives dispatchers the same control-first rule as the writer. Separate
// bounded queues keep data pressure from blocking pump admission; this method
// keeps a consumer from accidentally undoing that guarantee with a fair select.
func (router *RoleRouter) Next(ctx context.Context) (RouteEvent, error) {
	if router == nil {
		return RouteEvent{}, ErrNilRuntimeDependency
	}
	if !router.nextActive.CompareAndSwap(false, true) {
		return RouteEvent{}, ErrRouterConsumerBusy
	}
	defer router.nextActive.Store(false)
	if err := router.nextError(ctx); err != nil {
		return RouteEvent{}, err
	}
	if router.controlBurst >= RouterMaximumControlBurst {
		select {
		case event := <-router.data:
			router.retireQueuedData(event)
			if err := router.nextError(ctx); err != nil {
				return RouteEvent{}, err
			}
			router.controlBurst = 0
			return event, nil
		default:
		}
	}
	if err := router.nextError(ctx); err != nil {
		return RouteEvent{}, err
	}
	select {
	case event := <-router.control:
		if err := router.nextError(ctx); err != nil {
			return RouteEvent{}, err
		}
		router.controlBurst++
		return event, nil
	default:
	}
	if err := router.nextError(ctx); err != nil {
		return RouteEvent{}, err
	}
	select {
	case event := <-router.data:
		router.retireQueuedData(event)
		if err := router.nextError(ctx); err != nil {
			return RouteEvent{}, err
		}
		router.controlBurst = 0
		return event, nil
	default:
	}
	select {
	case <-router.done:
		return RouteEvent{}, ErrSessionTerminated
	case <-ctx.Done():
		return RouteEvent{}, ctx.Err()
	case event := <-router.control:
		if err := router.nextError(ctx); err != nil {
			return RouteEvent{}, err
		}
		router.controlBurst++
		return event, nil
	case event := <-router.data:
		router.retireQueuedData(event)
		if err := router.nextError(ctx); err != nil {
			return RouteEvent{}, err
		}
		router.controlBurst = 0
		return event, nil
	}
}

func (router *RoleRouter) nextError(ctx context.Context) error {
	select {
	case <-router.done:
		return ErrSessionTerminated
	default:
	}
	return ctx.Err()
}

func (router *RoleRouter) InboundDirection() Direction {
	direction, _ := router.role.InboundDirection()
	return direction
}

func (router *RoleRouter) OutboundDirection() Direction {
	direction, _ := router.role.OutboundDirection()
	return direction
}

func (router *RoleRouter) AdmitOutbound(
	message Message,
	permit OutboundOperationPermit,
) (OutboundAdmission, error) {
	return router.operations.AdmitOutbound(router.OutboundDirection(), message, permit)
}

func (router *RoleRouter) AcceptOutboundReplay(
	message Message,
	permit OutboundReplayPermit,
) (OutboundAdmission, error) {
	return router.operations.AcceptOutboundReplay(router.OutboundDirection(), message, permit)
}

func (router *RoleRouter) AcceptOutboundTerminal() error {
	if router.role != RoleSender {
		return ErrInvalidDirection
	}
	return router.operations.TerminateLocal()
}

func (router *RoleRouter) TerminateLocal() error {
	if router == nil {
		return ErrNilRuntimeDependency
	}
	return router.operations.TerminateLocal()
}

func (router *RoleRouter) RegisterHandler(kind MessageKind, handler MessageHandler) error {
	if router == nil || handler == nil {
		return ErrNilRuntimeDependency
	}
	if kind == MessageSessionTerminal {
		// The pump returns a terminal directly so backlog can never hide it.
		return ErrInvalidDirection
	}
	if err := validateKindDirection(router.InboundDirection(), kind); err != nil {
		return err
	}
	router.lifecycleMu.RLock()
	defer router.lifecycleMu.RUnlock()
	if router.closed {
		return ErrSessionTerminated
	}
	router.handlerMu.Lock()
	defer router.handlerMu.Unlock()
	if _, exists := router.handlers[kind]; exists {
		return ErrHandlerRegistered
	}
	router.handlers[kind] = handler
	return nil
}

// Dispatch resolves one already-authenticated bounded RouteEvent. Keeping this
// explicit lets a session facade choose worker ownership without creating a
// hidden goroutine or allowing handlers to compete with Control/Data consumers.
func (router *RoleRouter) Dispatch(ctx context.Context, event RouteEvent) error {
	if router == nil {
		return ErrNilRuntimeDependency
	}
	router.lifecycleMu.RLock()
	defer router.lifecycleMu.RUnlock()
	if router.closed {
		return ErrSessionTerminated
	}
	if event.err != nil {
		return event.err
	}
	if !event.hasMessage {
		return ErrInvalidRouteEvent
	}
	router.handlerMu.RLock()
	handler := router.handlers[event.message.kind]
	router.handlerMu.RUnlock()
	if handler == nil {
		return fmt.Errorf("%w: %d", ErrHandlerMissing, event.message.kind)
	}
	return handler.HandleMessage(RetainMessageContext(ctx, event.messageContext), event.message)
}

func (router *RoleRouter) RouteInbound(
	ctx context.Context,
	message Message,
) (OperationDisposition, error) {
	router.lifecycleMu.RLock()
	defer router.lifecycleMu.RUnlock()
	if router.closed {
		return OperationDrop, ErrSessionTerminated
	}
	// Admission and publication form one cross-lane commit. Without this lock a
	// later CANCEL could mutate lifecycle state and enter the control queue before
	// the request whose generation it canceled.
	router.ingressMu.Lock()
	defer router.ingressMu.Unlock()
	admission, err := router.operations.ObserveInbound(router.InboundDirection(), message)
	disposition := admission.Disposition
	if err != nil || disposition == OperationDrop {
		admission.continuation.rollback()
		return disposition, err
	}
	if disposition == OperationSessionTerminal {
		// The pump returns the authenticated terminal itself, so it cannot be
		// hidden behind ordinary control backlog.
		return disposition, nil
	}
	settled := false
	defer func() {
		if !settled {
			admission.continuation.rollback()
		}
	}()

	operationID, _ := message.OperationID()
	if !admission.Generation.IsZero() {
		ctx = WithOperationGeneration(ctx, admission.Generation)
	}
	if !admission.Outbound.IsZero() {
		ctx = WithOutboundOperationPermit(ctx, admission.Outbound)
	}
	event := RouteEvent{
		message: message, hasMessage: true, operationID: operationID,
		generation: admission.Generation, messageContext: ctx,
	}
	if message.IsData() {
		disposition, err := router.enqueueData(event)
		if err == nil {
			admission.continuation.commit()
			settled = true
		}
		return disposition, err
	}
	if operationFinalMustFollowData(message.Kind()) {
		deferred, err := router.enqueueFinalAfterData(event)
		if err != nil || deferred {
			if err == nil {
				admission.continuation.commit()
				settled = true
			}
			return disposition, err
		}
	}
	select {
	case router.control <- event:
		admission.continuation.commit()
		settled = true
		return disposition, nil
	default:
		return OperationDrop, ErrRouterControlFull
	}
}

func (router *RoleRouter) RouteAuthenticatedOperationViolation(
	_ context.Context,
	message Message,
	violation AuthenticatedOperationViolation,
) (bool, error) {
	if router == nil {
		return false, ErrNilRuntimeDependency
	}
	router.lifecycleMu.RLock()
	defer router.lifecycleMu.RUnlock()
	if router.closed {
		return false, ErrSessionTerminated
	}
	// Violation routing shares ingress serialization with ordinary admission so
	// the observer is selected from one exact active-or-tombstoned generation.
	router.ingressMu.Lock()
	defer router.ingressMu.Unlock()
	return router.operations.RecordAuthenticatedOperationViolation(message, violation)
}

// Close is the quiescent ownership boundary used after ingress pumps and the
// sole dispatch consumer have joined. It releases queued message contexts and
// handler references before a runtime publishes Done.
func (router *RoleRouter) Close() {
	if router == nil {
		return
	}
	router.lifecycleMu.Lock()
	defer router.lifecycleMu.Unlock()
	if router.closed {
		return
	}
	router.closed = true
	close(router.done)
	_ = router.operations.TerminateLocal()

	for {
		select {
		case <-router.control:
		default:
			goto controlDrained
		}
	}

controlDrained:
	router.queueMu.Lock()
	for {
		select {
		case <-router.data:
		default:
			clear(router.pendingData)
			router.queueMu.Unlock()
			goto dataDrained
		}
	}

dataDrained:
	router.handlerMu.Lock()
	clear(router.handlers)
	router.handlerMu.Unlock()
}

func (router *RoleRouter) enqueueData(event RouteEvent) (OperationDisposition, error) {
	router.queueMu.Lock()
	event.tracksData = true
	router.pendingData[event.generation]++
	select {
	case router.data <- event:
		router.queueMu.Unlock()
		return OperationDeliver, nil
	default:
		router.decrementPendingDataLocked(event.generation)
		router.queueMu.Unlock()
		generation, _ := OperationGenerationFromContext(event.messageContext, event.operationID)
		return router.handleDataOverflow(event.operationID, generation)
	}
}

func (router *RoleRouter) enqueueFinalAfterData(event RouteEvent) (bool, error) {
	router.queueMu.Lock()
	defer router.queueMu.Unlock()
	if router.pendingData[event.generation] == 0 {
		return false, nil
	}
	select {
	case router.data <- event:
		return true, nil
	default:
		// Delivering a final ahead of an admitted fragment would make a valid
		// record appear truncated. Closing the session is safer than publishing
		// an operation result whose authenticated order was changed locally.
		return false, ErrRouterDataFull
	}
}

func (router *RoleRouter) retireQueuedData(event RouteEvent) {
	if !event.tracksData {
		return
	}
	router.queueMu.Lock()
	router.decrementPendingDataLocked(event.generation)
	router.queueMu.Unlock()
}

func (router *RoleRouter) decrementPendingDataLocked(generation OperationGeneration) {
	remaining := router.pendingData[generation]
	if remaining <= 1 {
		delete(router.pendingData, generation)
		return
	}
	router.pendingData[generation] = remaining - 1
}

func operationFinalMustFollowData(kind MessageKind) bool {
	return kind == MessageOperationComplete || kind == MessageOperationError
}

func (router *RoleRouter) handleDataOverflow(
	operationID OperationID,
	generation OperationGeneration,
) (OperationDisposition, error) {
	if err := router.operations.CancelGeneration(generation); err != nil {
		return OperationDrop, err
	}
	overflow := RouteEvent{operationID: operationID, err: ErrRouterDataFull}
	select {
	case router.control <- overflow:
		return OperationDrop, nil
	default:
		return OperationDrop, ErrRouterControlFull
	}
}
