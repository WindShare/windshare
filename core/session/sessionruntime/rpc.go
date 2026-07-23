package sessionruntime

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const operationResponseFrames = 512

const operationIDPrefixAttempts = 4

var (
	ErrOperationMissing         = errors.New("session runtime operation is not active")
	ErrOperationOverflow        = errors.New("session runtime operation response queue is full")
	ErrOperationIDExhausted     = errors.New("session runtime operation identity space is exhausted")
	errRequestNotDelivered      = errors.New("session runtime request was not delivered")
	errContinuationNotDelivered = errors.New("session runtime continuation was not delivered")
	errRPCOperationAuthority    = errors.New("session runtime request admission returned incomplete operation authority")
)

type rpcRequestSendError struct {
	outcome  protocolsession.SendOutcome
	admitted bool
	cause    error
}

func (failure *rpcRequestSendError) Error() string {
	return fmt.Sprintf("send operation request: %v", failure.cause)
}

func (failure *rpcRequestSendError) Unwrap() error { return failure.cause }

func requestProvenNotDelivered(err error) bool {
	var failure *rpcRequestSendError
	return errors.As(err, &failure) && failure.outcome == protocolsession.SendOutcomeDropped && !failure.admitted
}

func newRPCRequestSendError(outcome protocolsession.SendOutcome, admitted bool, cause error) error {
	if cause == nil {
		cause = ErrRuntimeClosed
	}
	return &rpcRequestSendError{outcome: outcome, admitted: admitted, cause: cause}
}

func rpcDeliveryError(runtime *runtimeCore, notDelivered error, cause error) error {
	if runtime.ctx.Err() != nil {
		return errors.Join(notDelivered, cause, ErrRuntimeClosed, runtime.Err())
	}
	return errors.Join(notDelivered, cause)
}

type operationIDSource struct {
	mu          sync.Mutex
	random      io.Reader
	prefix      [8]byte
	counter     uint64
	initialized bool
	exhausted   bool
}

func (source *operationIDSource) New() (protocolsession.OperationID, error) {
	if source == nil || source.random == nil {
		return protocolsession.OperationID{}, ErrRuntimeConfig
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if !source.initialized {
		for attempt := 0; attempt < operationIDPrefixAttempts; attempt++ {
			if _, err := io.ReadFull(source.random, source.prefix[:]); err != nil {
				return protocolsession.OperationID{}, err
			}
			if !allZero(source.prefix[:]) {
				source.initialized = true
				break
			}
		}
		if !source.initialized {
			return protocolsession.OperationID{}, ErrRuntimeConfig
		}
	}
	if source.exhausted {
		return protocolsession.OperationID{}, ErrOperationIDExhausted
	}
	source.counter++
	if source.counter == ^uint64(0) {
		source.exhausted = true
	}
	encoded := make([]byte, protocolsession.IdentityBytes)
	copy(encoded, source.prefix[:])
	binary.BigEndian.PutUint64(encoded[len(source.prefix):], source.counter)
	return protocolsession.OperationIDFromBytes(encoded)
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

type rpcClient struct {
	runtime *runtimeCore
	ids     operationIDSource

	mu     sync.Mutex
	closed bool
	calls  map[protocolsession.OperationID]*operationCall
}

func newRPCClient(runtime *runtimeCore, random io.Reader) *rpcClient {
	return &rpcClient{
		runtime: runtime, ids: operationIDSource{random: random},
		calls: make(map[protocolsession.OperationID]*operationCall),
	}
}

func (client *rpcClient) HandleMessage(ctx context.Context, message protocolsession.Message) error {
	id, ok := message.OperationID()
	if !ok {
		return ErrOperationMissing
	}
	client.mu.Lock()
	call := client.calls[id]
	client.mu.Unlock()
	if call == nil {
		// OperationTable already classifies authenticated late finals and
		// fragments; a locally cancelled caller may safely have released its sink.
		return nil
	}
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, id)
	if !ok {
		return nil
	}
	expected, _ := call.operationAuthority()
	if expected.IsZero() {
		if !generation.IsCurrent() {
			return nil
		}
	} else if !expected.Same(generation) {
		return nil
	}
	return call.enqueue(operationResponse{message: message, generation: generation})
}

func (client *rpcClient) begin(
	ctx context.Context,
	kind protocolsession.MessageKind,
	body []byte,
) (*operationCall, error) {
	return client.beginOn(ctx, nil, kind, body)
}

func (client *rpcClient) beginOn(
	ctx context.Context,
	lane *LaneIdentity,
	kind protocolsession.MessageKind,
	body []byte,
) (*operationCall, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := client.ids.New()
	if err != nil {
		return nil, err
	}
	message, err := protocolsession.NewMessage(kind, &id, body)
	if err != nil {
		return nil, err
	}
	call := &operationCall{
		id: id, messages: make(chan operationResponse, operationResponseFrames), done: make(chan struct{}),
	}
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return nil, ErrRuntimeClosed
	}
	if client.calls[id] != nil {
		client.mu.Unlock()
		return nil, ErrOperationMissing
	}
	client.calls[id] = call
	client.mu.Unlock()
	selected, err := client.runtime.lanes.selectLane(lane)
	if err != nil {
		client.end(call)
		return nil, newRPCRequestSendError(protocolsession.SendOutcomeDropped, false, err)
	}
	call.lane = selected.identity
	var receipt protocolsession.SendReceipt
	if kind == protocolsession.MessagePeerOffer {
		receipt, err = selected.writer.TryControlObservingAuthenticatedViolations(
			message,
			call.observeAuthenticatedOperationViolation,
		)
	} else {
		receipt, err = selected.writer.TryControl(message)
	}
	if err != nil {
		client.end(call)
		return nil, newRPCRequestSendError(protocolsession.SendOutcomeDropped, false, err)
	}
	completion := receipt.Await(ctx)
	outcome, err := completion.Outcome, completion.Err
	exactAuthority := completion.Admitted && !completion.Generation.IsZero() &&
		!completion.Operation.IsZero() && completion.Generation.Same(completion.Operation.Generation())
	safePreadmissionDrop := !completion.Admitted && outcome == protocolsession.SendOutcomeDropped
	if !exactAuthority && !safePreadmissionDrop {
		authorityErr := client.runtime.failRPCOperationAuthority()
		client.end(call)
		return nil, newRPCRequestSendError(
			outcome, completion.Admitted,
			rpcDeliveryError(client.runtime, errRequestNotDelivered, authorityErr),
		)
	}
	if exactAuthority && !call.setAuthority(completion.Generation, completion.Operation) {
		client.end(call)
		return nil, newRPCRequestSendError(
			outcome, true,
			rpcDeliveryError(client.runtime, errRequestNotDelivered, ErrRuntimeClosed),
		)
	}
	if err != nil || outcome != protocolsession.SendOutcomeDelivered {
		if outcome == protocolsession.SendOutcomeUnknown && ctx.Err() == nil && client.runtime.ctx.Err() == nil {
			if !completion.Settled || !call.setRequestReplay(message, completion.Replay) {
				authorityErr := client.runtime.failRPCOperationAuthority()
				client.end(call)
				return nil, newRPCRequestSendError(
					outcome, completion.Admitted,
					rpcDeliveryError(client.runtime, errRequestNotDelivered, authorityErr),
				)
			}
			// Transport acceptance is ambiguous: the peer may already own this exact
			// operation. The exact request replay is retained only to order a later
			// dependent control on a replacement lane.
			return call, nil
		}
		var cleanupErr error
		if completion.Admitted {
			cleanupErr = client.admitCancellation(call, contentflow.CancelReasonTimeout)
		}
		client.end(call)
		return nil, newRPCRequestSendError(
			outcome, completion.Admitted,
			rpcDeliveryError(client.runtime, errRequestNotDelivered, errors.Join(err, cleanupErr)),
		)
	}
	return call, nil
}

func (runtime *runtimeCore) failRPCOperationAuthority() error {
	_ = runtime.router.TerminateLocal()
	runtime.recordError(errRPCOperationAuthority)
	runtime.cancel()
	return errRPCOperationAuthority
}

func (runtime *runtimeCore) reconcileLocalCancel(
	generation protocolsession.OperationGeneration,
) error {
	err := runtime.operations.CancelGeneration(generation)
	if err == nil {
		return nil
	}
	// Failure to retain a cancellation tombstone would leak active authority and
	// make a later ID collision ambiguous. Fail-closing atomically clears the
	// table instead of continuing a session whose at-most-once state is unknown.
	_ = runtime.router.TerminateLocal()
	runtime.recordError(err)
	runtime.cancel()
	return err
}

func (client *rpcClient) sendContinuation(
	ctx context.Context,
	call *operationCall,
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.SendOutcome, error) {
	if call == nil || call.id.IsZero() {
		return protocolsession.SendOutcomeDropped, ErrOperationMissing
	}
	if kind == protocolsession.MessagePeerCandidate {
		release, err := call.acquireCandidateSend(ctx, client.runtime.ctx)
		if err != nil {
			return protocolsession.SendOutcomeDropped, err
		}
		defer release()
	}
	client.mu.Lock()
	active := client.calls[call.id] == call
	client.mu.Unlock()
	if !active {
		return protocolsession.SendOutcomeDropped, ErrOperationMissing
	}
	message, err := protocolsession.NewMessage(kind, &call.id, body)
	if err != nil {
		return protocolsession.SendOutcomeDropped, err
	}
	var retryCause error
	for attempt := 0; attempt < 2; attempt++ {
		result := client.sendContinuationAttempt(ctx, call, message, kind, attempt, retryCause)
		if result.retry {
			retryCause = result.retryCause
			continue
		}
		return result.outcome, result.err
	}
	return protocolsession.SendOutcomeDropped, errors.Join(retryCause, errContinuationNotDelivered)
}

type continuationAttemptResult struct {
	outcome    protocolsession.SendOutcome
	err        error
	retry      bool
	retryCause error
}

func (client *rpcClient) sendContinuationAttempt(
	ctx context.Context,
	call *operationCall,
	message protocolsession.Message,
	kind protocolsession.MessageKind,
	attempt int,
	retryCause error,
) continuationAttemptResult {
	selected, err := client.continuationLane(call)
	if err != nil {
		return continuationAttemptResult{
			outcome: protocolsession.SendOutcomeDropped, err: errors.Join(retryCause, err),
		}
	}
	_, authority := call.operationAuthority()
	if authority.IsZero() {
		return continuationAttemptResult{outcome: protocolsession.SendOutcomeDropped, err: ErrOperationMissing}
	}
	receipt, err := selected.writer.TryAuthorizedControl(message, authority)
	if err != nil {
		if client.retryContinuationAdmission(ctx, attempt, selected, err) {
			return continuationAttemptResult{retry: true, retryCause: err}
		}
		return continuationAttemptResult{
			outcome: protocolsession.SendOutcomeDropped, err: errors.Join(retryCause, err),
		}
	}
	if kind == protocolsession.MessageCancel && receipt.Admitted() {
		// CANCEL commits the local tombstone synchronously; physical delivery is
		// deliberately best effort and must not hold cancellation completion open.
		return continuationAttemptResult{outcome: protocolsession.SendOutcomeUnknown}
	}
	completion := receipt.Await(ctx)
	if client.retryContinuationCompletion(ctx, attempt, selected, completion) {
		return continuationAttemptResult{retry: true, retryCause: completion.Err}
	}
	if kind == protocolsession.MessageCancel && completion.Outcome == protocolsession.SendOutcomeDropped && completion.Err == nil {
		return continuationAttemptResult{outcome: completion.Outcome}
	}
	if !completion.Admitted && completion.Outcome == protocolsession.SendOutcomeDropped && completion.Err == nil {
		// Exact continuation replay is a successful idempotent no-op. It must
		// remain distinguishable from a pre-transport failure with a cause.
		return continuationAttemptResult{outcome: completion.Outcome}
	}
	if completion.Err != nil || completion.Outcome != protocolsession.SendOutcomeDelivered {
		return continuationAttemptResult{
			outcome: completion.Outcome,
			err: rpcDeliveryError(
				client.runtime, errContinuationNotDelivered, errors.Join(retryCause, completion.Err),
			),
		}
	}
	return continuationAttemptResult{outcome: completion.Outcome}
}

func (client *rpcClient) retryContinuationAdmission(
	ctx context.Context,
	attempt int,
	selected selectedLane,
	err error,
) bool {
	if attempt != 0 || !errors.Is(err, protocolsession.ErrWriterStopped) ||
		ctx.Err() != nil || client.runtime.ctx.Err() != nil {
		return false
	}
	client.runtime.lanes.markSelectedClosing(selected)
	return true
}

func (client *rpcClient) retryContinuationCompletion(
	ctx context.Context,
	attempt int,
	selected selectedLane,
	completion protocolsession.SendCompletion,
) bool {
	if attempt != 0 || !completion.Settled || completion.Admitted ||
		completion.Outcome != protocolsession.SendOutcomeDropped || !completion.RetryableAcrossLane ||
		ctx.Err() != nil || client.runtime.ctx.Err() != nil {
		return false
	}
	client.runtime.lanes.markSelectedClosing(selected)
	return true
}

func (client *rpcClient) continuationLane(call *operationCall) (selectedLane, error) {
	call.laneMu.Lock()
	defer call.laneMu.Unlock()
	selected, err := client.runtime.lanes.selectLane(&call.lane)
	if err == nil {
		return selected, nil
	}
	selected, err = client.runtime.lanes.selectLane(nil)
	if err != nil {
		return selectedLane{}, err
	}
	if err := call.queueRequestReplay(selected.writer); err != nil {
		return selectedLane{}, err
	}
	// A continuation migrates only after the authenticated original lane is no
	// longer usable. An ambiguous initial request is queued first on the same
	// control FIFO so a candidate cannot overtake its dependency. CANCEL remains
	// a preemptive race signal and may retire the operation before replay delivery.
	call.lane = selected.identity
	return selected, nil
}

func (client *rpcClient) end(call *operationCall) {
	if call == nil {
		return
	}
	if client == nil {
		call.close()
		return
	}
	client.mu.Lock()
	if client.calls[call.id] == call {
		delete(client.calls, call.id)
	}
	client.mu.Unlock()
	call.close()
}

func (client *rpcClient) Close() {
	if client == nil {
		return
	}
	client.mu.Lock()
	client.closed = true
	calls := make([]*operationCall, 0, len(client.calls))
	for _, call := range client.calls {
		calls = append(calls, call)
	}
	clear(client.calls)
	client.mu.Unlock()
	for _, call := range calls {
		call.close()
	}
}

func (client *rpcClient) cancelAndEnd(call *operationCall, reason contentflow.CancelReason) error {
	if call == nil {
		return nil
	}
	// The exact-generation tombstone must be committed before the response sink
	// disappears. Otherwise a local decoder/observer failure can leave an active
	// remote operation with no caller capable of consuming or cancelling it.
	err := client.admitCancellation(call, reason)
	client.end(call)
	return err
}

func (client *rpcClient) await(ctx context.Context, call *operationCall) (protocolsession.Message, error) {
	return client.awaitCall(ctx, call, true)
}

func (client *rpcClient) awaitPeer(
	ctx context.Context,
	call *operationCall,
) (protocolsession.Message, error) {
	return client.awaitCall(ctx, call, false)
}

func (client *rpcClient) awaitCall(
	ctx context.Context,
	call *operationCall,
	cancelOnContext bool,
) (protocolsession.Message, error) {
	if client == nil || client.runtime == nil {
		return protocolsession.Message{}, ErrRuntimeClosed
	}
	if call == nil {
		return protocolsession.Message{}, ErrOperationMissing
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		done := call.doneChannel()
		select {
		case <-ctx.Done():
			if !cancelOnContext {
				return protocolsession.Message{}, ctx.Err()
			}
			// Cancellation admission is part of the observable failure. Dropping
			// it here would make a later exact-call cleanup appear benign even
			// when the first cancellation transition genuinely failed.
			return protocolsession.Message{}, errors.Join(
				ctx.Err(),
				client.admitCancellation(call, contentflow.CancelReasonTimeout),
			)
		case <-client.runtime.Done():
			return protocolsession.Message{}, errors.Join(ErrRuntimeClosed, client.runtime.Err())
		case <-done:
			return protocolsession.Message{}, ErrOperationMissing
		case response := <-call.messages:
			expected, _ := call.operationAuthority()
			if expected.Same(response.generation) {
				return response.message, nil
			}
		}
	}
}

func (client *rpcClient) admitCancellation(
	call *operationCall,
	reason contentflow.CancelReason,
) error {
	if client == nil || client.runtime == nil {
		return ErrRuntimeClosed
	}
	if call == nil {
		return ErrOperationMissing
	}
	generation, authority := call.operationAuthority()
	if generation.IsZero() || authority.IsZero() {
		return ErrOperationMissing
	}
	body, err := contentflow.EncodeCancelReason(reason)
	if err != nil {
		return errors.Join(err, client.runtime.reconcileLocalCancel(generation))
	}
	message, err := protocolsession.NewMessage(protocolsession.MessageCancel, &call.id, body)
	if err != nil {
		return errors.Join(err, client.runtime.reconcileLocalCancel(generation))
	}
	selected, err := client.continuationLane(call)
	if err == nil {
		_, err = selected.writer.TryAuthorizedControl(message, authority)
	}
	if err == nil {
		// TryAuthorizedControl admits CANCEL synchronously or proves that the
		// generation is already retired. Wire delivery remains best effort.
		return nil
	}
	return errors.Join(err, client.runtime.reconcileLocalCancel(generation))
}

func finalizeLocalCancelIfDropped(
	runtime *runtimeCore,
	generation protocolsession.OperationGeneration,
	outcome protocolsession.SendOutcome,
) error {
	if outcome != protocolsession.SendOutcomeDropped {
		return nil
	}
	return runtime.reconcileLocalCancel(generation)
}

func (client *rpcClient) register(router *protocolsession.RoleRouter) error {
	for _, kind := range []protocolsession.MessageKind{
		protocolsession.MessageCatalogResult,
		protocolsession.MessageOpenResults,
		protocolsession.MessageLeaseResult,
		protocolsession.MessageBlockFragment,
		protocolsession.MessageOperationComplete,
		protocolsession.MessageOperationError,
		protocolsession.MessageScanProgress,
		protocolsession.MessageLaneAttach,
		protocolsession.MessagePeerAnswer,
		protocolsession.MessagePeerCandidate,
	} {
		if err := router.RegisterHandler(kind, client); err != nil {
			return fmt.Errorf("register receiver response %d: %w", kind, err)
		}
	}
	return nil
}
