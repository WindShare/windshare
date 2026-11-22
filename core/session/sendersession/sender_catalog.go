package sendersession

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type catalogHandler struct {
	service  *catalogflow.AddressedSenderService
	outbound *sessioncore.Responses
	queue    chan catalogOperation
	workers  chan struct{}

	queueMu  sync.Mutex
	stopping bool
	mu       sync.Mutex
	active   map[protocolsession.OperationRef]context.CancelFunc
}

type catalogOperation struct {
	op      protocolsession.Operation
	message protocolsession.Message
}

func newCatalogHandler(service *catalogflow.AddressedSenderService, outbound *sessioncore.Responses) *catalogHandler {
	return &catalogHandler{
		service: service, outbound: outbound, queue: make(chan catalogOperation, 256),
		workers: make(chan struct{}, catalogflow.MaxConcurrentDirectoryLoads),
		active:  make(map[protocolsession.OperationRef]context.CancelFunc),
	}
}

func (handler *catalogHandler) HandleMessage(
	ctx context.Context,
	op protocolsession.Operation,
	message protocolsession.Message,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if message.Kind() != protocolsession.MessageListChildren && message.Kind() != protocolsession.MessageCancel {
		return contentflow.ErrUnexpectedMessage
	}
	operationID, ok := message.OperationID()
	if !ok || op.IsZero() || op.ID() != operationID {
		return contentflow.ErrOperationIdentity
	}
	if message.Kind() == protocolsession.MessageCancel {
		if !op.IsCurrent() {
			return nil
		}
	} else if !op.IsActive() {
		return nil
	}
	handler.queueMu.Lock()
	defer handler.queueMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if handler.stopping {
		return contentflow.ErrServiceClosed
	}
	select {
	case handler.queue <- catalogOperation{op: op, message: message}:
		return nil
	default:
		return contentflow.ErrServiceQueueFull
	}
}

func (handler *catalogHandler) Run(ctx context.Context) error {
	var wait sync.WaitGroup
	defer func() {
		handler.markStopping()
		handler.cancelAll()
		wait.Wait()
		handler.drainQueue()
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case queued := <-handler.queue:
			if queued.message.Kind() == protocolsession.MessageCancel {
				handler.cancel(queued.op.Ref())
				continue
			}
			if !queued.op.IsActive() {
				continue
			}
			operationContext, cancel := context.WithCancel(ctx)
			if !handler.add(queued.op.Ref(), cancel) {
				cancel()
				continue
			}
			wait.Go(func() {
				select {
				case handler.workers <- struct{}{}:
					defer func() { <-handler.workers }()
				case <-operationContext.Done():
					handler.remove(queued.op.Ref())
					return
				}
				handler.process(operationContext, queued.op, queued.message.Body())
				handler.remove(queued.op.Ref())
			})
		}
	}
}

func (handler *catalogHandler) process(ctx context.Context, op protocolsession.Operation, body []byte) {
	err := handler.serve(ctx, op, body)
	if err != nil && !errors.Is(err, context.Canceled) {
		_ = handler.outbound.SendOperationError(ctx, op, classifyCatalogOperationFailure(err))
	}
}

func classifyCatalogOperationFailure(err error) contentflow.OperationFailure {
	if errors.Is(err, catalog.ErrBudgetExceeded) {
		return contentflow.OperationFailure{
			Scope:      protocolsession.OperationScopeDirectory,
			Code:       catalogflow.DirectoryCodeBudget,
			Retryable:  true,
			RetryAfter: catalog.DefaultScanRetryCooldown,
			Message:    "Catalog resource budget exceeded",
		}
	}
	return contentflow.OperationFailure{
		Scope:   protocolsession.OperationScopeDirectory,
		Code:    catalogflow.DirectoryCodePermanentIO,
		Message: "Catalog operation failed",
	}
}

func (handler *catalogHandler) serve(
	ctx context.Context,
	op protocolsession.Operation,
	body []byte,
) error {
	request, err := catalogflow.DecodeListRequest(body)
	if err != nil {
		return err
	}
	payload, err := handler.service.Serve(ctx, request, handler.progressObserver(op))
	if err != nil {
		return err
	}
	result, err := catalogflow.EncodeCatalogResult(payload)
	if err != nil {
		return err
	}
	err = handler.outbound.SendControl(ctx, op, protocolsession.MessageCatalogResult, result)
	if errors.Is(err, protocolsession.ErrOperationSuppressed) {
		return nil
	}
	return err
}

func (handler *catalogHandler) progressObserver(op protocolsession.Operation) catalog.ScanProgressObserver {
	return catalog.ScanProgressObserverFunc(func(
		ctx context.Context,
		update catalog.ScanProgress,
	) error {
		body, err := protocolsession.EncodeScanProgress(protocolsession.ScanProgress{
			AttemptID: update.AttemptID, DiscoveredEntries: update.DiscoveredEntries,
		})
		if err != nil {
			return err
		}
		err = handler.outbound.SendControl(ctx, op, protocolsession.MessageScanProgress, body)
		if errors.Is(err, protocolsession.ErrOperationSuppressed) {
			return context.Canceled
		}
		return err
	})
}

func (handler *catalogHandler) cancel(ref protocolsession.OperationRef) {
	handler.mu.Lock()
	cancel := handler.active[ref]
	handler.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (handler *catalogHandler) add(ref protocolsession.OperationRef, cancel context.CancelFunc) bool {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.active[ref] != nil {
		return false
	}
	handler.active[ref] = cancel
	return true
}

func (handler *catalogHandler) remove(ref protocolsession.OperationRef) {
	handler.mu.Lock()
	delete(handler.active, ref)
	handler.mu.Unlock()
}

func (handler *catalogHandler) cancelAll() {
	handler.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(handler.active))
	for _, cancel := range handler.active {
		cancels = append(cancels, cancel)
	}
	clear(handler.active)
	handler.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (handler *catalogHandler) markStopping() {
	handler.queueMu.Lock()
	handler.stopping = true
	handler.queueMu.Unlock()
}

func (handler *catalogHandler) drainQueue() {
	handler.queueMu.Lock()
	defer handler.queueMu.Unlock()
	for {
		select {
		case <-handler.queue:
		default:
			return
		}
	}
}
