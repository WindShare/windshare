package sessioncore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/core/session"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

var (
	errInvalidRouteEvent = errors.New("sessioncore: route event is invalid")
	errQueueConsumerBusy = errors.New("sessioncore: inbound queue Next already has a consumer")
)

type routeEvent struct {
	message    protocolsession.Message
	hasMessage bool
	op         protocolsession.Operation
	lane       session.LaneIdentity
	observedAt time.Time
	err        error
	tracksData bool
}

// inboundLaneHandler is implemented by receiver Requests so dispatch passes
// authenticated lane and receive-time metadata directly without context.WithValue.
type inboundLaneHandler interface {
	handleInboundMessage(
		ctx context.Context,
		op protocolsession.Operation,
		message protocolsession.Message,
		lane session.LaneIdentity,
		observedAt time.Time,
	) error
}

// inboundQueue owns bounded control/data dispatch queues, final-after-data
// ordering, and deferred sender request promotion when operation capacity is full.
type inboundQueue struct {
	runtime    *Runtime
	role       protocolsession.Role
	operations *operationTable
	control    chan routeEvent
	data       chan routeEvent
	done       chan struct{}

	handlerMu sync.RWMutex
	handlers  map[protocolsession.MessageKind]protocolsession.MessageHandler

	queueMu             sync.Mutex
	pendingData         map[protocolsession.OperationID]uint32
	ingressMu           sync.Mutex
	deferred            map[protocolsession.OperationID][]routeEvent
	deferredOrder       []protocolsession.OperationID
	deferredFrames      int
	deferredBytes       int
	deferredCancelBytes int
	admissionWake       chan struct{}

	lifecycleMu sync.RWMutex
	closed      bool

	nextActive   atomic.Bool
	controlBurst int
}

func newInboundQueue(
	runtime *Runtime,
	role protocolsession.Role,
	operations *operationTable,
	limits protocolsession.RouterLimits,
) (*inboundQueue, error) {
	if _, err := role.InboundDirection(); err != nil {
		return nil, err
	}
	if operations == nil {
		return nil, fmt.Errorf("%w: operation table", protocolsession.ErrNilRuntimeDependency)
	}
	if limits.ControlFrames <= 0 || limits.ControlFrames > protocolsession.RouterControlQueueLimit ||
		limits.DataFrames <= 0 || limits.DataFrames > protocolsession.RouterDataQueueLimit {
		return nil, fmt.Errorf("sessioncore: invalid router limits: %+v", limits)
	}
	return &inboundQueue{
		runtime:       runtime,
		role:          role,
		operations:    operations,
		control:       make(chan routeEvent, limits.ControlFrames),
		data:          make(chan routeEvent, limits.DataFrames),
		done:          make(chan struct{}),
		handlers:      make(map[protocolsession.MessageKind]protocolsession.MessageHandler),
		pendingData:   make(map[protocolsession.OperationID]uint32),
		deferred:      make(map[protocolsession.OperationID][]routeEvent),
		admissionWake: make(chan struct{}, 1),
	}, nil
}

func (q *inboundQueue) inboundDirection() protocolsession.Direction {
	dir, _ := q.role.InboundDirection()
	return dir
}

func (q *inboundQueue) RegisterHandler(
	kind protocolsession.MessageKind,
	handler protocolsession.MessageHandler,
) error {
	if q == nil || handler == nil {
		return protocolsession.ErrNilRuntimeDependency
	}
	if kind == protocolsession.MessageSessionTerminal {
		return protocolsession.ErrInvalidDirection
	}
	if err := protocolsession.ValidateKindDirection(q.inboundDirection(), kind); err != nil {
		return err
	}
	q.lifecycleMu.RLock()
	defer q.lifecycleMu.RUnlock()
	if q.closed {
		return protocolsession.ErrSessionTerminated
	}
	q.handlerMu.Lock()
	defer q.handlerMu.Unlock()
	if _, exists := q.handlers[kind]; exists {
		return protocolsession.ErrHandlerRegistered
	}
	q.handlers[kind] = handler
	return nil
}

func (q *inboundQueue) RouteInbound(
	lane session.LaneIdentity,
	message protocolsession.Message,
) (protocolsession.OperationDisposition, error) {
	q.lifecycleMu.RLock()
	defer q.lifecycleMu.RUnlock()
	if q.closed {
		return protocolsession.OperationDrop, protocolsession.ErrSessionTerminated
	}
	q.ingressMu.Lock()
	defer q.ingressMu.Unlock()
	observedAt := time.Time{}
	if q.runtime != nil && q.role == protocolsession.RoleReceiver &&
		message.Kind() == protocolsession.MessageOperationError &&
		q.runtime.protocolOperationTracingEnabled() {
		observedAt = q.runtime.now()
	}
	if q.shouldDeferInbound(message) {
		return q.deferInboundLocked(lane, observedAt, message)
	}
	disposition, err := q.routeAdmittedLocked(lane, observedAt, message)
	if protocolsession.IsOperationCapacityError(err) {
		return q.deferInboundLocked(lane, observedAt, message)
	}
	return disposition, err
}

func (q *inboundQueue) routeAdmittedLocked(
	lane session.LaneIdentity,
	observedAt time.Time,
	message protocolsession.Message,
) (protocolsession.OperationDisposition, error) {
	admission, err := q.operations.ObserveInbound(q.inboundDirection(), lane, message)
	if err != nil || admission.disposition == protocolsession.OperationDrop {
		return admission.disposition, err
	}
	if admission.disposition == protocolsession.OperationSessionTerminal {
		return admission.disposition, nil
	}
	event := routeEvent{
		message:    message,
		hasMessage: true,
		op:         admission.op,
		lane:       lane,
		observedAt: observedAt,
	}
	if message.IsData() {
		disposition, enqueueErr := q.enqueueData(event)
		if enqueueErr != nil && admission.undo != nil {
			q.operations.mu.Lock()
			admission.undo()
			q.operations.mu.Unlock()
		}
		return disposition, enqueueErr
	}
	if operationFinalMustFollowData(message.Kind()) {
		deferred, enqueueErr := q.enqueueFinalAfterData(event)
		if enqueueErr != nil {
			if admission.undo != nil {
				q.operations.mu.Lock()
				admission.undo()
				q.operations.mu.Unlock()
			}
			return protocolsession.OperationDrop, enqueueErr
		}
		if deferred {
			q.traceSenderRequestReceived(lane, message)
			return admission.disposition, nil
		}
	}
	select {
	case q.control <- event:
		q.traceSenderRequestReceived(lane, message)
		return admission.disposition, nil
	default:
		if admission.undo != nil {
			q.operations.mu.Lock()
			admission.undo()
			q.operations.mu.Unlock()
		}
		return protocolsession.OperationDrop, protocolsession.ErrRouterControlFull
	}
}

func (q *inboundQueue) traceSenderRequestReceived(
	lane session.LaneIdentity,
	message protocolsession.Message,
) {
	if q.runtime == nil || q.role != protocolsession.RoleSender ||
		!receiverRequestKind(message.Kind()) ||
		message.Kind() == protocolsession.MessageRequestBlocks ||
		!q.runtime.protocolOperationTracingEnabled() {
		return
	}
	id, _ := message.OperationID()
	q.runtime.traceProtocolOperation(sessionobs.ProtocolOperationObservation{
		Stage:                  sessionobs.ProtocolOperationSenderRequestReceived,
		OperationID:            id,
		RequestKind:            message.Kind(),
		Lane:                   lane,
		HasLane:                true,
		UsableLanesAtSelection: q.runtime.lanes.usableCount(),
		Cause:                  sessionobs.ProtocolOperationCauseNone,
	})
}

func (q *inboundQueue) RouteAuthenticatedOperationViolation(
	message protocolsession.Message,
	violation protocolsession.AuthenticatedOperationViolation,
) (bool, error) {
	if q == nil {
		return false, protocolsession.ErrNilRuntimeDependency
	}
	q.lifecycleMu.RLock()
	defer q.lifecycleMu.RUnlock()
	if q.closed {
		return false, protocolsession.ErrSessionTerminated
	}
	q.ingressMu.Lock()
	defer q.ingressMu.Unlock()
	return q.operations.RecordAuthenticatedOperationViolation(message, violation)
}

func (q *inboundQueue) Next(ctx context.Context) (routeEvent, error) {
	if q == nil {
		return routeEvent{}, protocolsession.ErrNilRuntimeDependency
	}
	if !q.nextActive.CompareAndSwap(false, true) {
		return routeEvent{}, errQueueConsumerBusy
	}
	defer q.nextActive.Store(false)
	for {
		capacity, err := q.promoteDeferred()
		if err != nil {
			return routeEvent{}, err
		}
		event, ready, err := q.nextEvent(ctx, capacity)
		if ready || err != nil {
			return event, err
		}
	}
}

func (q *inboundQueue) nextEvent(ctx context.Context, capacity operationCapacity) (routeEvent, bool, error) {
	if err := q.nextError(ctx); err != nil {
		return routeEvent{}, true, err
	}
	if q.controlBurst >= protocolsession.RouterMaximumControlBurst {
		select {
		case event := <-q.data:
			q.retireQueuedData(event)
			if err := q.nextError(ctx); err != nil {
				return routeEvent{}, true, err
			}
			q.controlBurst = 0
			return event, true, nil
		default:
		}
	}
	if err := q.nextError(ctx); err != nil {
		return routeEvent{}, true, err
	}
	select {
	case event := <-q.control:
		if err := q.nextError(ctx); err != nil {
			return routeEvent{}, true, err
		}
		q.controlBurst++
		return event, true, nil
	default:
	}
	if err := q.nextError(ctx); err != nil {
		return routeEvent{}, true, err
	}
	select {
	case event := <-q.data:
		q.retireQueuedData(event)
		if err := q.nextError(ctx); err != nil {
			return routeEvent{}, true, err
		}
		q.controlBurst = 0
		return event, true, nil
	default:
	}
	select {
	case <-q.admissionWake:
		return routeEvent{}, false, nil
	case <-capacity.changed:
		return routeEvent{}, false, nil
	case <-q.done:
		return routeEvent{}, true, protocolsession.ErrSessionTerminated
	case <-ctx.Done():
		return routeEvent{}, true, ctx.Err()
	case event := <-q.control:
		if err := q.nextError(ctx); err != nil {
			return routeEvent{}, true, err
		}
		q.controlBurst++
		return event, true, nil
	case event := <-q.data:
		q.retireQueuedData(event)
		if err := q.nextError(ctx); err != nil {
			return routeEvent{}, true, err
		}
		q.controlBurst = 0
		return event, true, nil
	}
}

func (q *inboundQueue) nextError(ctx context.Context) error {
	select {
	case <-q.done:
		return protocolsession.ErrSessionTerminated
	default:
	}
	return ctx.Err()
}

func (q *inboundQueue) Dispatch(ctx context.Context, event routeEvent) error {
	if q == nil {
		return protocolsession.ErrNilRuntimeDependency
	}
	q.lifecycleMu.RLock()
	defer q.lifecycleMu.RUnlock()
	if q.closed {
		return protocolsession.ErrSessionTerminated
	}
	if event.err != nil {
		return event.err
	}
	if !event.hasMessage {
		return errInvalidRouteEvent
	}
	q.handlerMu.RLock()
	handler := q.handlers[event.message.Kind()]
	q.handlerMu.RUnlock()
	if handler == nil {
		return fmt.Errorf("%w: %d", protocolsession.ErrHandlerMissing, event.message.Kind())
	}
	if direct, ok := handler.(inboundLaneHandler); ok {
		return direct.handleInboundMessage(ctx, event.op, event.message, event.lane, event.observedAt)
	}
	return handler.HandleMessage(ctx, event.op, event.message)
}

func (q *inboundQueue) Close() {
	if q == nil {
		return
	}
	q.lifecycleMu.Lock()
	defer q.lifecycleMu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.done)
	_ = q.operations.TerminateLocal()
	clear(q.deferred)
	q.deferredOrder = nil
	q.deferredFrames = 0
	q.deferredBytes = 0
	q.deferredCancelBytes = 0
	for {
		select {
		case <-q.control:
		default:
			goto controlDrained
		}
	}
controlDrained:
	q.queueMu.Lock()
	for {
		select {
		case <-q.data:
		default:
			clear(q.pendingData)
			q.queueMu.Unlock()
			goto dataDrained
		}
	}
dataDrained:
	q.handlerMu.Lock()
	clear(q.handlers)
	q.handlerMu.Unlock()
}

func (q *inboundQueue) enqueueData(event routeEvent) (protocolsession.OperationDisposition, error) {
	q.queueMu.Lock()
	event.tracksData = true
	id := event.op.ID()
	q.pendingData[id]++
	select {
	case q.data <- event:
		q.queueMu.Unlock()
		return protocolsession.OperationDeliver, nil
	default:
		q.decrementPendingDataLocked(id)
		q.queueMu.Unlock()
		if _, err := q.operations.CancelOperation(id); err != nil {
			return protocolsession.OperationDrop, err
		}
		select {
		case q.control <- routeEvent{op: event.op, err: protocolsession.ErrRouterDataFull}:
			return protocolsession.OperationDrop, nil
		default:
			return protocolsession.OperationDrop, protocolsession.ErrRouterControlFull
		}
	}
}

func (q *inboundQueue) enqueueFinalAfterData(event routeEvent) (bool, error) {
	q.queueMu.Lock()
	defer q.queueMu.Unlock()
	if q.pendingData[event.op.ID()] == 0 {
		return false, nil
	}
	select {
	case q.data <- event:
		return true, nil
	default:
		// Delivering a final ahead of an admitted fragment would make a valid
		// record appear truncated. Fail the session instead of reordering.
		return false, protocolsession.ErrRouterDataFull
	}
}

func (q *inboundQueue) retireQueuedData(event routeEvent) {
	if !event.tracksData {
		return
	}
	q.queueMu.Lock()
	q.decrementPendingDataLocked(event.op.ID())
	q.queueMu.Unlock()
}

func (q *inboundQueue) decrementPendingDataLocked(id protocolsession.OperationID) {
	remaining := q.pendingData[id]
	if remaining <= 1 {
		delete(q.pendingData, id)
		return
	}
	q.pendingData[id] = remaining - 1
}

func operationFinalMustFollowData(kind protocolsession.MessageKind) bool {
	return kind == protocolsession.MessageOperationComplete || kind == protocolsession.MessageOperationError
}

func (q *inboundQueue) shouldDeferInbound(message protocolsession.Message) bool {
	id, ok := message.OperationID()
	if !ok || len(q.deferredOrder) == 0 {
		return false
	}
	if len(q.deferred[id]) != 0 {
		return true
	}
	if !deferredRequest(message.Kind()) && message.Kind() != protocolsession.MessageCancel {
		return false
	}
	exists, terminal := q.operations.hasCurrentID(id)
	if terminal {
		return false
	}
	return !exists
}

func (q *inboundQueue) deferInboundLocked(
	lane session.LaneIdentity,
	observedAt time.Time,
	message protocolsession.Message,
) (protocolsession.OperationDisposition, error) {
	if err := protocolsession.ValidateKindDirection(q.inboundDirection(), message.Kind()); err != nil {
		return protocolsession.OperationDrop, err
	}
	id, ok := message.OperationID()
	if !ok || q.role != protocolsession.RoleSender || message.IsData() {
		return protocolsession.OperationDrop, protocolsession.ErrInvalidOperationID
	}
	pending := q.deferred[id]
	for _, event := range pending {
		if message.Kind() == protocolsession.MessageCancel && event.message.Kind() == protocolsession.MessageCancel {
			return protocolsession.OperationDrop, nil
		}
		if deferredRequest(message.Kind()) && deferredRequest(event.message.Kind()) {
			if event.message.Kind() == message.Kind() &&
				event.message.OperationFingerprint(q.inboundDirection()) == message.OperationFingerprint(q.inboundDirection()) {
				return protocolsession.OperationDrop, nil
			}
			return protocolsession.OperationDrop, protocolsession.ErrOperationIDReused
		}
	}
	consumesFrame := deferredConsumesFrame(pending, message.Kind())
	if consumesFrame && q.deferredFrames >= cap(q.control) {
		return protocolsession.OperationDrop, protocolsession.ErrRouterControlFull
	}
	bodyLen := len(message.Body())
	if message.Kind() == protocolsession.MessageCancel {
		if bodyLen > protocolsession.ControlQueueByteLimit-q.deferredCancelBytes {
			return protocolsession.OperationDrop, protocolsession.ErrRouterControlFull
		}
	} else if bodyLen > protocolsession.ControlQueueByteLimit-q.deferredBytes {
		return protocolsession.OperationDrop, protocolsession.ErrRouterControlFull
	}
	if len(pending) == 0 {
		q.deferredOrder = append(q.deferredOrder, id)
	}
	q.deferred[id] = append(pending, routeEvent{
		message:    message,
		hasMessage: true,
		lane:       lane,
		observedAt: observedAt,
	})
	if consumesFrame {
		q.deferredFrames++
	}
	if message.Kind() == protocolsession.MessageCancel {
		q.deferredCancelBytes += bodyLen
	} else {
		q.deferredBytes += bodyLen
	}
	select {
	case q.admissionWake <- struct{}{}:
	default:
	}
	return protocolsession.OperationDeliver, nil
}

func (q *inboundQueue) promoteDeferred() (operationCapacity, error) {
	q.lifecycleMu.RLock()
	defer q.lifecycleMu.RUnlock()
	if q.closed {
		return operationCapacity{}, protocolsession.ErrSessionTerminated
	}
	q.ingressMu.Lock()
	defer q.ingressMu.Unlock()
	for len(q.deferredOrder) != 0 {
		id := q.deferredOrder[0]
		pending := q.deferred[id]
		cancelled := deferredCancelled(pending)
		dispatchFrames := len(pending)
		if cancelled {
			dispatchFrames = 1
		}
		if dispatchFrames > cap(q.control)-len(q.control) {
			return operationCapacity{}, nil
		}
		capacity, err := q.operations.capacity()
		if err != nil || !capacity.available {
			return capacity, err
		}
		promoted, err := q.promoteDeferredHistory(pending, cancelled)
		if err != nil {
			return operationCapacity{}, err
		}
		if promoted {
			q.retireDeferred(id, pending)
		}
	}
	return operationCapacity{}, nil
}

func (q *inboundQueue) promoteDeferredHistory(pending []routeEvent, cancelled bool) (bool, error) {
	for index, event := range pending {
		var err error
		if !cancelled || event.message.Kind() == protocolsession.MessageCancel {
			_, err = q.routeAdmittedLocked(event.lane, event.observedAt, event.message)
		} else {
			_, err = q.operations.ObserveInbound(q.inboundDirection(), event.lane, event.message)
		}
		if index == 0 && protocolsession.IsOperationCapacityError(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func (q *inboundQueue) retireDeferred(id protocolsession.OperationID, pending []routeEvent) {
	delete(q.deferred, id)
	q.deferredOrder = q.deferredOrder[1:]
	for position, retired := range pending {
		if deferredConsumesFrame(pending[:position], retired.message.Kind()) {
			q.deferredFrames--
		}
		if retired.message.Kind() == protocolsession.MessageCancel {
			q.deferredCancelBytes -= len(retired.message.Body())
		} else {
			q.deferredBytes -= len(retired.message.Body())
		}
	}
}

func deferredRequest(kind protocolsession.MessageKind) bool {
	return kind.IsRequest() || kind == protocolsession.MessageLaneAttach
}

func deferredConsumesFrame(pending []routeEvent, kind protocolsession.MessageKind) bool {
	if len(pending) == 0 {
		return true
	}
	return kind != protocolsession.MessageCancel &&
		(pending[0].message.Kind() != protocolsession.MessageCancel || !deferredRequest(kind))
}

func deferredCancelled(events []routeEvent) bool {
	for _, event := range events {
		if event.message.Kind() == protocolsession.MessageCancel {
			return true
		}
	}
	return false
}
