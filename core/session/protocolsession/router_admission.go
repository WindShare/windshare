package protocolsession

import "context"

func (router *RoleRouter) shouldDeferInbound(message Message) bool {
	id, ok := message.OperationID()
	if !ok || len(router.deferredOrder) == 0 {
		return false
	}
	table := router.operations
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.terminal {
		return false
	}
	if len(router.deferred[id]) != 0 {
		return true
	}
	if !deferredRequest(message.kind) && message.kind != MessageCancel {
		return false
	}
	table.pruneExpired()
	// Fresh arrivals cannot repeatedly take reclaimed slots ahead of callers
	// already waiting. Established operations still bypass the admission inbox.
	return table.operationAuthority(id) == nil
}

// A peer can release its retained identity slightly before this side releases
// its last writer pin. Keep that bounded ingress backlog outside the operation
// table and keep pumping established operations, especially CANCEL. The sole
// dispatcher promotes waiting work when capacity becomes available.
func (router *RoleRouter) deferInboundLocked(ctx context.Context, message Message) (OperationDisposition, error) {
	if err := validateKindDirection(router.InboundDirection(), message.kind); err != nil {
		return OperationDrop, err
	}
	id, ok := message.OperationID()
	if !ok || router.role != RoleSender || message.IsData() {
		return OperationDrop, ErrInvalidOperationID
	}
	pending := router.deferred[id]
	for _, event := range pending {
		if message.kind == MessageCancel && event.message.kind == MessageCancel {
			return OperationDrop, nil
		}
		if deferredRequest(message.kind) && deferredRequest(event.message.kind) {
			if event.message.kind == message.kind && event.message.operationFingerprint(router.InboundDirection()) == message.operationFingerprint(router.InboundDirection()) {
				return OperationDrop, nil
			}
			return OperationDrop, ErrOperationIDReused
		}
	}
	consumesFrame := deferredConsumesFrame(pending, message.kind)
	if consumesFrame && router.deferredFrames >= cap(router.control) {
		return OperationDrop, ErrRouterControlFull
	}
	if message.kind == MessageCancel {
		// Each waiting identity owns a cancellation position as well as its
		// request position. Filling the request inbox cannot prevent its owner
		// from abandoning work that has not begun.
		if len(message.body) > ControlQueueByteLimit-router.deferredCancelBytes {
			return OperationDrop, ErrRouterControlFull
		}
	} else {
		if len(message.body) > ControlQueueByteLimit-router.deferredBytes {
			return OperationDrop, ErrRouterControlFull
		}
	}
	if len(pending) == 0 {
		router.deferredOrder = append(router.deferredOrder, id)
	}
	router.deferred[id] = append(pending, RouteEvent{
		message: message, hasMessage: true, operationID: id, messageContext: ctx,
	})
	if consumesFrame {
		router.deferredFrames++
	}
	if message.kind == MessageCancel {
		router.deferredCancelBytes += len(message.body)
	} else {
		router.deferredBytes += len(message.body)
	}
	select {
	case router.admissionWake <- struct{}{}:
	default:
	}
	return OperationDeliver, nil
}

func (router *RoleRouter) promoteDeferred() (operationCapacity, error) {
	router.lifecycleMu.RLock()
	defer router.lifecycleMu.RUnlock()
	if router.closed {
		return operationCapacity{}, ErrSessionTerminated
	}
	router.ingressMu.Lock()
	defer router.ingressMu.Unlock()
	for len(router.deferredOrder) != 0 {
		id := router.deferredOrder[0]
		pending := router.deferred[id]
		cancelled := deferredCancelled(pending)
		dispatchFrames := len(pending)
		if cancelled {
			dispatchFrames = 1
		}
		if dispatchFrames > cap(router.control)-len(router.control) {
			return operationCapacity{}, nil
		}
		capacity, err := router.operations.capacity()
		if err != nil || !capacity.available {
			return capacity, err
		}
		promoted, err := router.promoteDeferredHistory(pending, cancelled)
		if err != nil {
			return operationCapacity{}, err
		}
		if promoted {
			router.retireDeferred(id, pending)
		}
	}
	return operationCapacity{}, nil
}

func (router *RoleRouter) promoteDeferredHistory(pending []RouteEvent, cancelled bool) (bool, error) {
	// Promote the entire history before dispatching any of it. A queued CANCEL
	// retires the generation before its request handler can start work.
	for index, event := range pending {
		err := router.promoteDeferredEvent(event, cancelled)
		if index == 0 && IsOperationCapacityError(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func (router *RoleRouter) retireDeferred(id OperationID, pending []RouteEvent) {
	delete(router.deferred, id)
	router.deferredOrder = router.deferredOrder[1:]
	for position, retired := range pending {
		if deferredConsumesFrame(pending[:position], retired.message.kind) {
			router.deferredFrames--
		}
		if retired.message.kind == MessageCancel {
			router.deferredCancelBytes -= len(retired.message.body)
		} else {
			router.deferredBytes -= len(retired.message.body)
		}
	}
}

func deferredRequest(kind MessageKind) bool { return kind.isRequest() || kind == MessageLaneAttach }

func deferredConsumesFrame(pending []RouteEvent, kind MessageKind) bool {
	if len(pending) == 0 {
		return true
	}
	// Request and cancel share one admission position in either arrival order.
	return kind != MessageCancel && (pending[0].message.kind != MessageCancel || !deferredRequest(kind))
}

func deferredCancelled(events []RouteEvent) bool {
	for _, event := range events {
		if event.message.kind == MessageCancel {
			return true
		}
	}
	return false
}

func (router *RoleRouter) promoteDeferredEvent(event RouteEvent, cancelled bool) error {
	if !cancelled || event.message.kind == MessageCancel {
		_, err := router.routeAdmittedLocked(event.messageContext, event.message)
		return err
	}
	// Validate the retained history but never publish abandoned service work.
	admission, err := router.operations.ObserveInbound(router.InboundDirection(), event.message)
	if err != nil {
		admission.continuation.rollback()
		return err
	}
	admission.continuation.commit()
	return nil
}
