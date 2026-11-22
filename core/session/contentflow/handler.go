package contentflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	RevisionErrorScope       = protocolsession.OperationScopeRevision
	BlockErrorScope          = protocolsession.OperationScopeBlock
	DefaultServiceQueueDepth = 256
	DefaultServiceWorkers    = 32
)

type OperationFailure = protocolsession.OperationFailure

// SemanticOutbound keeps operation routing and authentication at the session
// boundary. SendControl takes a canonical CBOR body.
type SemanticOutbound interface {
	SendControl(ctx context.Context, op protocolsession.Operation, kind protocolsession.MessageKind, body []byte) error
	SendFragment(ctx context.Context, op protocolsession.Operation, message protocolsession.Message) error
	SendOperationError(ctx context.Context, op protocolsession.Operation, failure OperationFailure) error
}

type SenderHandlerConfig struct {
	Service        *SenderService
	Outbound       SemanticOutbound
	QueueDepth     int
	Workers        int
	DecisionTracer SenderDecisionTracer
}

type queuedOperation struct {
	op      protocolsession.Operation
	message protocolsession.Message
}

type SenderHandler struct {
	service        *SenderService
	outbound       SemanticOutbound
	queue          chan queuedOperation
	workers        chan struct{}
	decisionTracer SenderDecisionTracer

	started  atomic.Bool
	queueMu  sync.Mutex
	stopping bool
	mu       sync.Mutex
	active   map[protocolsession.OperationRef]context.CancelFunc
}

// SenderHandlerLifecycle is a point-in-time view of operation ownership.
type SenderHandlerLifecycle struct {
	ActiveOperations int
	RunningWorkers   int
	QueuedOperations int
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
		decisionTracer: config.DecisionTracer,
		active:         make(map[protocolsession.OperationRef]context.CancelFunc),
	}, nil
}

func (h *SenderHandler) LifecycleSnapshot() SenderHandlerLifecycle {
	if h == nil {
		return SenderHandlerLifecycle{}
	}
	h.mu.Lock()
	active := len(h.active)
	h.mu.Unlock()
	return SenderHandlerLifecycle{
		ActiveOperations: active,
		RunningWorkers:   len(h.workers),
		QueuedOperations: len(h.queue),
	}
}

func (h *SenderHandler) HandleMessage(
	ctx context.Context,
	op protocolsession.Operation,
	message protocolsession.Message,
) error {
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
	if !ok || op.IsZero() || op.ID() != operationID {
		return ErrOperationIdentity
	}
	if kind == protocolsession.MessageCancel {
		if _, err := DecodeCancelReason(message.Body()); err != nil {
			return err
		}
		if !op.IsCurrent() {
			return nil
		}
	} else if !op.IsActive() {
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
	case h.queue <- queuedOperation{op: op, message: message}:
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
				h.cancel(queued.op.Ref())
				continue
			}
			if !queued.op.IsActive() {
				continue
			}
			operationContext, cancel := context.WithCancel(ctx)
			if !h.register(queued.op.Ref(), cancel) {
				cancel()
				continue
			}
			operations.Go(func() {
				select {
				case h.workers <- struct{}{}:
					defer func() { <-h.workers }()
				case <-operationContext.Done():
					h.unregister(queued.op.Ref())
					return
				}
				h.process(operationContext, queued.op, queued.message)
				h.unregister(queued.op.Ref())
			})
		}
	}
}

func (h *SenderHandler) process(
	ctx context.Context,
	op protocolsession.Operation,
	message protocolsession.Message,
) {
	var err error
	switch message.Kind() {
	case protocolsession.MessageOpenRevisions:
		err = h.processOpen(ctx, op, message.Body())
	case protocolsession.MessageRenewLease:
		err = h.processRenew(ctx, op, message.Body())
	case protocolsession.MessageReleaseLease:
		err = h.processRelease(ctx, op, message.Body())
	case protocolsession.MessageRequestBlocks:
		err = h.processBlocks(ctx, op, message.Body())
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
	_ = h.outbound.SendOperationError(ctx, op, failure)
}

func (h *SenderHandler) processOpen(ctx context.Context, op protocolsession.Operation, body []byte) error {
	operationID := op.ID()
	request, err := DecodeOpenRequest(body)
	if err != nil {
		return err
	}
	results, err := h.service.Open(ctx, request)
	if err != nil {
		return wrapServiceError("open revisions", err)
	}
	h.traceCapacityDecisions(operationID, results)
	encoded, err := EncodeOpenResults(results)
	if err != nil {
		return errors.Join(err, h.endOpenResults(operationID, results, content.LeaseUndelivered))
	}
	if err := h.outbound.SendControl(ctx, op, protocolsession.MessageOpenResults, encoded); err != nil {
		if protocolsession.IsNotSent(err) {
			return errors.Join(wrapOutboundError(err), h.endOpenResults(operationID, results, content.LeaseUndelivered))
		}
		return errors.Join(wrapOutboundError(err), h.endOpenResults(operationID, results, content.LeaseDetached))
	}
	return nil
}

func (h *SenderHandler) processRenew(ctx context.Context, op protocolsession.Operation, body []byte) error {
	operationID := op.ID()
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
		h.service.detachLease(leaseID)
		h.traceLeaseDecision(operationID, protocolsession.MessageRenewLease, leaseID, content.LeaseDetached)
		return err
	}
	err = h.outbound.SendControl(ctx, op, protocolsession.MessageLeaseResult, encoded)
	if err != nil {
		h.service.detachLease(leaseID)
		h.traceLeaseDecision(operationID, protocolsession.MessageRenewLease, leaseID, content.LeaseDetached)
	}
	return wrapOutboundError(err)
}

func (h *SenderHandler) processRelease(ctx context.Context, op protocolsession.Operation, body []byte) error {
	leaseID, err := DecodeLeaseRequest(body)
	if err != nil {
		return err
	}
	if err := h.service.Release(leaseID); err != nil {
		return wrapServiceError("release lease", err)
	}
	h.traceLeaseDecision(op.ID(), protocolsession.MessageReleaseLease, leaseID, content.LeaseRelinquished)
	encoded, _ := EncodeOperationComplete(0)
	err = h.outbound.SendControl(ctx, op, protocolsession.MessageOperationComplete, encoded)
	return wrapOutboundError(err)
}

func (h *SenderHandler) processBlocks(ctx context.Context, op protocolsession.Operation, body []byte) error {
	operationID := op.ID()
	request, err := DecodeBlockRequest(body)
	if err != nil {
		return err
	}
	count, err := h.service.ServeBlocks(ctx, operationID, request, func(sendContext context.Context, message protocolsession.Message) error {
		if sendErr := h.outbound.SendFragment(sendContext, op, message); sendErr != nil {
			return &fragmentOutboundFailure{cause: wrapOutboundError(sendErr)}
		}
		return nil
	})
	if err != nil {
		h.traceBlockLeaseRejection(operationID, request.leaseID, err)
		return wrapServiceError("serve blocks", err)
	}
	encoded, _ := EncodeOperationComplete(count)
	err = h.outbound.SendControl(ctx, op, protocolsession.MessageOperationComplete, encoded)
	return wrapOutboundError(err)
}

func wrapOutboundError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	return errors.Join(ErrOutboundUnavailable, err)
}

func (h *SenderHandler) register(ref protocolsession.OperationRef, cancel context.CancelFunc) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.active[ref]; exists {
		return false
	}
	h.active[ref] = cancel
	return true
}

func (h *SenderHandler) unregister(ref protocolsession.OperationRef) {
	h.mu.Lock()
	delete(h.active, ref)
	h.mu.Unlock()
}

func (h *SenderHandler) cancel(ref protocolsession.OperationRef) {
	h.mu.Lock()
	cancel := h.active[ref]
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

func RegisterSenderHandlers(
	router interface {
		RegisterHandler(protocolsession.MessageKind, protocolsession.MessageHandler) error
	},
	handler *SenderHandler,
) error {
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
