package contentflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	RevisionErrorScope       = protocolsession.OperationScopeRevision
	BlockErrorScope          = protocolsession.OperationScopeBlock
	DefaultServiceQueueDepth = 256
	DefaultServiceWorkers    = 32
)

type OperationFailure = protocolsession.OperationFailure

// SemanticOutbound keeps sender-control construction inside the lane writer.
// SendControl must add key 255 and sign with the exact writer-assigned sequence
// atomically before envelope sealing; bodyWithoutSignature is canonical CBOR.
type SemanticOutbound interface {
	SendControl(ctx context.Context, kind protocolsession.MessageKind, operationID protocolsession.OperationID, bodyWithoutSignature []byte) (protocolsession.SendOutcome, error)
	SendFragment(ctx context.Context, message protocolsession.Message) error
	SendOperationError(ctx context.Context, operationID protocolsession.OperationID, failure OperationFailure) error
}

type SenderHandlerConfig struct {
	Service    *SenderService
	Outbound   SemanticOutbound
	QueueDepth int
	Workers    int
}

type queuedOperation struct {
	ctx       context.Context
	message   protocolsession.Message
	operation handlerOperation
}

type SenderHandler struct {
	service  *SenderService
	outbound SemanticOutbound
	queue    chan queuedOperation
	workers  chan struct{}

	started  atomic.Bool
	queueMu  sync.Mutex
	stopping bool
	mu       sync.Mutex
	active   map[handlerOperation]context.CancelFunc
}

// SenderHandlerLifecycle is a point-in-time view of operation ownership. It is
// intentionally limited to counts: callers can diagnose drain boundaries without
// acquiring the generation capabilities stored in the handler's active map.
type SenderHandlerLifecycle struct {
	ActiveOperations int
	RunningWorkers   int
	QueuedOperations int
}

type handlerOperation struct {
	id         protocolsession.OperationID
	generation protocolsession.OperationGeneration
}

func operationFromContext(
	ctx context.Context,
	id protocolsession.OperationID,
) (handlerOperation, bool) {
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, id)
	if !ok || generation.IsZero() {
		return handlerOperation{}, false
	}
	return handlerOperation{id: id, generation: generation}, true
}

type fragmentOutboundFailure struct{ cause error }

func (failure *fragmentOutboundFailure) Error() string { return failure.cause.Error() }
func (failure *fragmentOutboundFailure) Unwrap() error { return failure.cause }

func NewSenderHandler(config SenderHandlerConfig) (*SenderHandler, error) {
	if config.Service == nil || config.Outbound == nil {
		return nil, errors.New("content sender handler requires service and semantic outbound path")
	}
	if config.QueueDepth == 0 {
		config.QueueDepth = DefaultServiceQueueDepth
	}
	if config.Workers == 0 {
		config.Workers = DefaultServiceWorkers
	}
	if config.QueueDepth < 0 || config.Workers < 0 {
		return nil, errors.New("content sender handler capacities must be positive")
	}
	return &SenderHandler{
		service: config.Service, outbound: config.Outbound,
		queue: make(chan queuedOperation, config.QueueDepth), workers: make(chan struct{}, config.Workers),
		active: make(map[handlerOperation]context.CancelFunc),
	}, nil
}

func (h *SenderHandler) LifecycleSnapshot() SenderHandlerLifecycle {
	if h == nil {
		return SenderHandlerLifecycle{}
	}
	h.mu.Lock()
	active := len(h.active)
	h.mu.Unlock()
	// Channel occupancy is concurrency-safe to sample and, unlike a goroutine
	// counter, exactly reflects ownership of the bounded worker/queue resources.
	return SenderHandlerLifecycle{
		ActiveOperations: active,
		RunningWorkers:   len(h.workers),
		QueuedOperations: len(h.queue),
	}
}

func (h *SenderHandler) HandleMessage(ctx context.Context, message protocolsession.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	kind := message.Kind()
	switch kind {
	case protocolsession.MessageOpenRevisions, protocolsession.MessageRenewLease,
		protocolsession.MessageReleaseLease, protocolsession.MessageRequestBlocks,
		protocolsession.MessageCancel:
	default:
		return ErrUnexpectedMessage
	}
	operationID, ok := message.OperationID()
	if !ok {
		return ErrOperationIdentity
	}
	if kind == protocolsession.MessageCancel {
		if _, err := DecodeCancelReason(message.Body()); err != nil {
			return err
		}
	}
	operation, ok := operationFromContext(ctx, operationID)
	if !ok {
		return ErrOperationIdentity
	}
	if kind == protocolsession.MessageCancel {
		if !operation.generation.IsCurrent() {
			return nil
		}
	} else if !operation.generation.IsActive() {
		return nil
	}
	h.queueMu.Lock()
	defer h.queueMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.stopping {
		return ErrServiceClosed
	}
	select {
	case h.queue <- queuedOperation{ctx: ctx, message: message, operation: operation}:
		return nil
	default:
		return ErrServiceQueueFull
	}
}

func (h *SenderHandler) Run(ctx context.Context) error {
	if !h.started.CompareAndSwap(false, true) {
		return errors.New("content sender handler may only run once")
	}
	var operations sync.WaitGroup
	defer func() {
		h.markStopping()
		h.cancelAll()
		operations.Wait()
		h.drainQueue()
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case queued := <-h.queue:
			if queued.message.Kind() == protocolsession.MessageCancel {
				h.cancel(queued.operation)
				continue
			}
			if !queued.operation.generation.IsActive() {
				continue
			}
			operationContext, cancel := context.WithCancel(
				protocolsession.RetainMessageContext(ctx, queued.ctx),
			)
			if !h.register(queued.operation, cancel) {
				cancel()
				continue
			}
			operations.Add(1)
			go func() {
				defer operations.Done()
				select {
				case h.workers <- struct{}{}:
					defer func() { <-h.workers }()
				case <-operationContext.Done():
					h.unregister(queued.operation)
					return
				}
				h.process(operationContext, queued.message)
				h.unregister(queued.operation)
			}()
		}
	}
}

func (h *SenderHandler) process(ctx context.Context, message protocolsession.Message) {
	operationID, _ := message.OperationID()
	var err error
	switch message.Kind() {
	case protocolsession.MessageOpenRevisions:
		err = h.processOpen(ctx, operationID, message.Body())
	case protocolsession.MessageRenewLease:
		err = h.processRenew(ctx, operationID, message.Body())
	case protocolsession.MessageReleaseLease:
		err = h.processRelease(ctx, operationID, message.Body())
	case protocolsession.MessageRequestBlocks:
		err = h.processBlocks(ctx, operationID, message.Body())
	}
	var fragmentFailure *fragmentOutboundFailure
	if err == nil || errors.Is(err, context.Canceled) ||
		(errors.Is(err, ErrOutboundUnavailable) && !errors.As(err, &fragmentFailure)) {
		return
	}
	failure := OperationFailure{Scope: RevisionErrorScope, Code: classifyRevisionError(err).Code, Message: "Revision operation failed"}
	if message.Kind() == protocolsession.MessageRequestBlocks && !errors.Is(err, errRevisionOperationScope) {
		failure = OperationFailure{Scope: BlockErrorScope, Code: classifyBlockError(err), Message: "Block operation failed"}
	}
	_ = h.outbound.SendOperationError(ctx, operationID, failure)
}

func (h *SenderHandler) processOpen(ctx context.Context, operationID protocolsession.OperationID, body []byte) error {
	request, err := DecodeOpenRequest(body)
	if err != nil {
		return err
	}
	results, err := h.service.Open(ctx, request)
	if err != nil {
		return wrapServiceError("open revisions", err)
	}
	encoded, err := EncodeOpenResults(results)
	if err != nil {
		return errors.Join(err, h.service.releaseOpenResults(results))
	}
	outcome, err := h.outbound.SendControl(ctx, protocolsession.MessageOpenResults, operationID, encoded)
	if outcome == protocolsession.SendOutcomeDropped {
		return errors.Join(wrapOutboundError(err), h.service.releaseOpenResults(results))
	}
	return wrapOutboundError(err)
}

func (h *SenderHandler) processRenew(ctx context.Context, operationID protocolsession.OperationID, body []byte) error {
	leaseID, err := DecodeLeaseRequest(body)
	if err != nil {
		return err
	}
	lease, err := h.service.Renew(leaseID)
	if err != nil {
		return wrapServiceError("renew lease", err)
	}
	encoded, err := EncodeLeaseResult(lease)
	if err != nil {
		h.service.retireLease(leaseID)
		return err
	}
	outcome, err := h.outbound.SendControl(ctx, protocolsession.MessageLeaseResult, operationID, encoded)
	if outcome == protocolsession.SendOutcomeDropped {
		h.service.retireLease(leaseID)
	}
	return wrapOutboundError(err)
}

func (h *SenderHandler) processRelease(ctx context.Context, operationID protocolsession.OperationID, body []byte) error {
	leaseID, err := DecodeLeaseRequest(body)
	if err != nil {
		return err
	}
	if err := h.service.Release(leaseID); err != nil {
		return wrapServiceError("release lease", err)
	}
	encoded, _ := EncodeOperationComplete(0)
	_, err = h.outbound.SendControl(ctx, protocolsession.MessageOperationComplete, operationID, encoded)
	return wrapOutboundError(err)
}

func (h *SenderHandler) processBlocks(ctx context.Context, operationID protocolsession.OperationID, body []byte) error {
	request, err := DecodeBlockRequest(body)
	if err != nil {
		return err
	}
	count, err := h.service.ServeBlocks(ctx, operationID, request, func(sendContext context.Context, message protocolsession.Message) error {
		sendContext = protocolsession.RetainMessageContext(sendContext, ctx)
		if sendErr := h.outbound.SendFragment(sendContext, message); sendErr != nil {
			// A fragment is non-final, so an OperationError remains the one legal
			// final that can release remote waiters and local operation authority.
			return &fragmentOutboundFailure{cause: wrapOutboundError(sendErr)}
		}
		return nil
	})
	if err != nil {
		return wrapServiceError("serve blocks", err)
	}
	encoded, _ := EncodeOperationComplete(count)
	_, err = h.outbound.SendControl(ctx, protocolsession.MessageOperationComplete, operationID, encoded)
	return wrapOutboundError(err)
}

func wrapOutboundError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	return errors.Join(ErrOutboundUnavailable, err)
}

func (h *SenderHandler) register(operation handlerOperation, cancel context.CancelFunc) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.active[operation]; exists {
		return false
	}
	h.active[operation] = cancel
	return true
}

func (h *SenderHandler) unregister(operation handlerOperation) {
	h.mu.Lock()
	delete(h.active, operation)
	h.mu.Unlock()
}

func (h *SenderHandler) cancel(operation handlerOperation) {
	h.mu.Lock()
	cancel := h.active[operation]
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *SenderHandler) cancelAll() {
	h.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(h.active))
	for _, cancel := range h.active {
		cancels = append(cancels, cancel)
	}
	clear(h.active)
	h.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (h *SenderHandler) markStopping() {
	h.queueMu.Lock()
	h.stopping = true
	h.queueMu.Unlock()
}

func (h *SenderHandler) drainQueue() {
	h.queueMu.Lock()
	defer h.queueMu.Unlock()
	for {
		select {
		case <-h.queue:
		default:
			return
		}
	}
}

func RegisterSenderHandlers(router *protocolsession.RoleRouter, handler *SenderHandler) error {
	if router == nil || handler == nil {
		return errors.New("content handler registration requires router and handler")
	}
	for _, kind := range []protocolsession.MessageKind{
		protocolsession.MessageOpenRevisions, protocolsession.MessageRenewLease,
		protocolsession.MessageReleaseLease, protocolsession.MessageRequestBlocks,
		protocolsession.MessageCancel,
	} {
		if err := router.RegisterHandler(kind, handler); err != nil {
			return fmt.Errorf("register content message kind %d: %w", kind, err)
		}
	}
	return nil
}
