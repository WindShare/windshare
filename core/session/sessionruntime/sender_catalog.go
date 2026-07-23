package sessionruntime

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type catalogHandler struct {
	service  *catalogflow.AddressedSenderService
	outbound senderOutbound
	queue    chan catalogOperation
	workers  chan struct{}

	queueMu  sync.Mutex
	stopping bool
	mu       sync.Mutex
	active   map[catalogOperationKey]context.CancelFunc
}

type catalogOperation struct {
	ctx       context.Context
	message   protocolsession.Message
	operation catalogOperationKey
}

type catalogOperationKey struct {
	id         protocolsession.OperationID
	generation protocolsession.OperationGeneration
}

type catalogHandlerLifecycle struct {
	activeOperations int
	runningWorkers   int
	queuedOperations int
}

func catalogKey(ctx context.Context, id protocolsession.OperationID) (catalogOperationKey, bool) {
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, id)
	if !ok || generation.IsZero() {
		return catalogOperationKey{}, false
	}
	return catalogOperationKey{id: id, generation: generation}, true
}

func newCatalogHandler(service *catalogflow.AddressedSenderService, outbound senderOutbound) *catalogHandler {
	return &catalogHandler{
		service: service, outbound: outbound, queue: make(chan catalogOperation, 256),
		workers: make(chan struct{}, catalogflow.MaxConcurrentDirectoryLoads),
		active:  make(map[catalogOperationKey]context.CancelFunc),
	}
}

func (handler *catalogHandler) lifecycleSnapshot() catalogHandlerLifecycle {
	if handler == nil {
		return catalogHandlerLifecycle{}
	}
	handler.mu.Lock()
	active := len(handler.active)
	handler.mu.Unlock()
	return catalogHandlerLifecycle{
		activeOperations: active,
		runningWorkers:   len(handler.workers),
		queuedOperations: len(handler.queue),
	}
}

func (handler *catalogHandler) HandleMessage(ctx context.Context, message protocolsession.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if message.Kind() != protocolsession.MessageListChildren && message.Kind() != protocolsession.MessageCancel {
		return contentflow.ErrUnexpectedMessage
	}
	operationID, ok := message.OperationID()
	if !ok {
		return contentflow.ErrOperationIdentity
	}
	operation, ok := catalogKey(ctx, operationID)
	if !ok {
		return contentflow.ErrOperationIdentity
	}
	if message.Kind() == protocolsession.MessageCancel {
		if !operation.generation.IsCurrent() {
			return nil
		}
	} else if !operation.generation.IsActive() {
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
	case handler.queue <- catalogOperation{ctx: ctx, message: message, operation: operation}:
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
				handler.cancel(queued.operation)
				continue
			}
			if !queued.operation.generation.IsActive() {
				continue
			}
			operationContext, cancel := context.WithCancel(
				protocolsession.RetainMessageContext(ctx, queued.ctx),
			)
			if !handler.add(queued.operation, cancel) {
				cancel()
				continue
			}
			wait.Go(func() {
				select {
				case handler.workers <- struct{}{}:
					defer func() { <-handler.workers }()
				case <-operationContext.Done():
					handler.remove(queued.operation)
					return
				}
				handler.process(operationContext, queued.operation.id, queued.message.Body())
				handler.remove(queued.operation)
			})
		}
	}
}

func (handler *catalogHandler) process(ctx context.Context, operationID protocolsession.OperationID, body []byte) {
	err := handler.serve(ctx, operationID, body)
	if err != nil && !errors.Is(err, context.Canceled) {
		_ = handler.outbound.SendOperationError(ctx, operationID, contentflow.OperationFailure{
			Scope: protocolsession.OperationScopeDirectory,
			Code:  catalogflow.DirectoryCodePermanentIO, Message: "Catalog operation failed",
		})
	}
}

func (handler *catalogHandler) serve(
	ctx context.Context,
	operationID protocolsession.OperationID,
	body []byte,
) error {
	request, err := catalogflow.DecodeListRequest(body)
	if err != nil {
		return err
	}
	object, err := handler.service.Serve(ctx, request, handler.progressObserver(ctx, operationID))
	if err != nil {
		return err
	}
	result, err := catalogflow.EncodeCatalogResult(object)
	if err != nil {
		return err
	}
	_, err = handler.outbound.SendControl(ctx, protocolsession.MessageCatalogResult, operationID, result)
	return err
}

func (handler *catalogHandler) progressObserver(
	operationContext context.Context,
	operationID protocolsession.OperationID,
) catalog.ScanProgressObserver {
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
		sendContext := protocolsession.RetainMessageContext(ctx, operationContext)
		_, err = handler.outbound.SendControl(sendContext, protocolsession.MessageScanProgress, operationID, body)
		return err
	})
}

func (handler *catalogHandler) cancel(operation catalogOperationKey) {
	handler.mu.Lock()
	cancel := handler.active[operation]
	handler.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (handler *catalogHandler) add(operation catalogOperationKey, cancel context.CancelFunc) bool {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.active[operation] != nil {
		return false
	}
	handler.active[operation] = cancel
	return true
}

func (handler *catalogHandler) remove(operation catalogOperationKey) {
	handler.mu.Lock()
	delete(handler.active, operation)
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
