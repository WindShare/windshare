package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

const remoteLeaseCleanupTimeout = 5 * time.Second

var ErrRemoteLeaseCollision = errors.New("sender reused an active lease identifier")

type remoteLeaseState struct {
	id         content.LeaseID
	ttl        time.Duration
	renewAfter time.Duration
	ctx        context.Context
	cancel     context.CancelFunc
	once       sync.Once

	mu  sync.Mutex
	err error
}

func (state *remoteLeaseState) close() {
	state.once.Do(func() {
		if state.cancel != nil {
			state.cancel()
		}
	})
}
func (state *remoteLeaseState) setError(err error) {
	state.mu.Lock()
	if state.err == nil {
		state.err = err
	}
	state.mu.Unlock()
}
func (state *remoteLeaseState) Err() error {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.err
}

type receiverRevisionClient struct {
	rpc       *rpcClient
	opener    RecordOpener
	chunkSize uint32
	after     func(time.Duration) <-chan time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	work      sync.WaitGroup

	mu     sync.Mutex
	closed bool
	leases map[content.LeaseID]*remoteLeaseState
}

func newReceiverRevisionClient(
	rpc *rpcClient,
	opener RecordOpener,
	chunkSize uint32,
	after func(time.Duration) <-chan time.Time,
) *receiverRevisionClient {
	ctx, cancel := context.WithCancel(rpc.runtime.ctx)
	return &receiverRevisionClient{
		rpc: rpc, opener: opener, chunkSize: chunkSize, after: after,
		ctx: ctx, cancel: cancel, leases: make(map[content.LeaseID]*remoteLeaseState),
	}
}

func (client *receiverRevisionClient) OpenRevision(
	ctx context.Context,
	fileID catalog.FileID,
) (transfer.OpenedRevision, error) {
	// RecordOpener has no context and may execute application-controlled crypto.
	// Runtime admission is therefore the ownership boundary that keeps its key
	// resources alive even when the session is cancelled from inside the callback.
	ctx, rpc, endAdmission, err := client.beginExternalOperation(ctx)
	if err != nil {
		return transfer.OpenedRevision{}, err
	}
	defer endAdmission()
	emptyRanges, err := content.NewRangeSet(nil)
	if err != nil {
		return transfer.OpenedRevision{}, err
	}
	request, err := contentflow.NewOpenRequest([]contentflow.OpenItem{{FileID: fileID, InitialRanges: emptyRanges}})
	if err != nil {
		return transfer.OpenedRevision{}, err
	}
	body, err := contentflow.EncodeOpenRequest(request)
	if err != nil {
		return transfer.OpenedRevision{}, err
	}
	call, err := rpc.begin(ctx, protocolsession.MessageOpenRevisions, body)
	if err != nil {
		return transfer.OpenedRevision{}, err
	}
	defer func() { _ = rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	message, err := rpc.await(ctx, call)
	if err != nil {
		return transfer.OpenedRevision{}, err
	}
	if message.Kind() == protocolsession.MessageOperationError {
		return transfer.OpenedRevision{}, remoteOperationErrorFor(message, protocolsession.OperationScopeRevision)
	}
	if message.Kind() != protocolsession.MessageOpenResults {
		return transfer.OpenedRevision{}, ErrOperationMissing
	}
	unsigned, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		return transfer.OpenedRevision{}, err
	}
	results, err := contentflow.DecodeOpenResults(unsigned, []catalog.FileID{fileID})
	if err != nil || len(results) != 1 {
		return transfer.OpenedRevision{}, errors.Join(ErrOperationMissing, err)
	}
	if results[0].Failure != nil {
		return transfer.OpenedRevision{}, &RemoteRevisionError{Failure: *results[0].Failure}
	}
	if lifecycleErr := client.operationLifecycleError(ctx); lifecycleErr != nil {
		return transfer.OpenedRevision{}, client.compensateRemoteLease(ctx, results[0].Lease.ID, lifecycleErr)
	}
	descriptor, err := client.opener.OpenRevision(fileID, client.chunkSize, results[0].RevisionObject)
	if err != nil {
		return transfer.OpenedRevision{}, client.compensateRemoteLease(ctx, results[0].Lease.ID, err)
	}
	if lifecycleErr := client.operationLifecycleError(ctx); lifecycleErr != nil {
		return transfer.OpenedRevision{}, client.compensateRemoteLease(ctx, results[0].Lease.ID, lifecycleErr)
	}
	opened, err := transfer.NewOpenedRevision(results[0].Lease.ID, descriptor)
	if err != nil {
		return transfer.OpenedRevision{}, client.compensateRemoteLease(ctx, results[0].Lease.ID, err)
	}
	leaseContext, stopLease := context.WithCancel(client.ctx)
	state := &remoteLeaseState{
		id: results[0].Lease.ID, ttl: results[0].Lease.TTL, renewAfter: results[0].Lease.RenewAfter,
		ctx: leaseContext, cancel: stopLease,
	}
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		state.close()
		return transfer.OpenedRevision{}, client.compensateRemoteLease(ctx, state.id, ErrRuntimeClosed)
	}
	if client.leases[state.id] != nil {
		client.mu.Unlock()
		state.close()
		return transfer.OpenedRevision{}, client.failRemoteLeaseCollision()
	}
	client.leases[state.id] = state
	client.work.Add(1)
	client.mu.Unlock()
	go func() {
		defer client.work.Done()
		client.renewLoop(state)
	}()
	return opened, nil
}

func (client *receiverRevisionClient) beginExternalOperation(
	ctx context.Context,
) (context.Context, *rpcClient, func(), error) {
	if client == nil {
		return nil, nil, nil, ErrRuntimeClosed
	}
	client.mu.Lock()
	if client.closed || client.rpc == nil {
		client.mu.Unlock()
		return nil, nil, nil, ErrRuntimeClosed
	}
	rpc := client.rpc
	client.mu.Unlock()
	operationContext, endAdmission, err := rpc.runtime.beginExternalAdmission(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	client.mu.Lock()
	closed := client.closed || client.rpc != rpc
	client.mu.Unlock()
	if closed {
		endAdmission()
		return nil, nil, nil, ErrRuntimeClosed
	}
	return operationContext, rpc, endAdmission, nil
}

func (client *receiverRevisionClient) operationLifecycleError(ctx context.Context) error {
	client.mu.Lock()
	closed := client.closed
	client.mu.Unlock()
	if closed || client.rpc.runtime.ctx.Err() != nil {
		return errors.Join(ErrRuntimeClosed, ctx.Err())
	}
	return ctx.Err()
}

func (client *receiverRevisionClient) compensateRemoteLease(
	ctx context.Context,
	leaseID content.LeaseID,
	cause error,
) error {
	// OPEN already completed remotely, so cancelling that operation cannot revoke
	// its lease. A bounded RELEASE transaction is the only exact compensation for
	// a local descriptor, wrapper, or registration failure.
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), remoteLeaseCleanupTimeout)
	cleanupErr := client.releaseRemoteLease(cleanupContext, leaseID)
	cancel()
	return errors.Join(cause, cleanupErr)
}

func (client *receiverRevisionClient) failRemoteLeaseCollision() error {
	// A duplicate authenticated identifier aliases an existing remote capability.
	// Releasing it as compensation would revoke the sibling lease, so fail-closing
	// is the only transition that preserves ownership until composite teardown.
	runtime := client.rpc.runtime
	_ = runtime.router.TerminateLocal()
	runtime.recordError(ErrRemoteLeaseCollision)
	runtime.cancel()
	return ErrRemoteLeaseCollision
}

type RemoteRevisionError struct{ Failure contentflow.RevisionFailure }

func (err *RemoteRevisionError) Error() string { return "sender could not open the file revision" }

func (client *receiverRevisionClient) ReleaseRevision(ctx context.Context, leaseID content.LeaseID) error {
	ctx, _, endAdmission, err := client.beginExternalOperation(ctx)
	if err != nil {
		return err
	}
	defer endAdmission()
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return ErrRuntimeClosed
	}
	state := client.leases[leaseID]
	delete(client.leases, leaseID)
	client.mu.Unlock()
	if state != nil {
		state.close()
	}
	return client.releaseRemoteLease(ctx, leaseID)
}

func (client *receiverRevisionClient) releaseRemoteLease(ctx context.Context, leaseID content.LeaseID) error {
	body, err := contentflow.EncodeLeaseRequest(leaseID)
	if err != nil {
		return err
	}
	call, err := client.rpc.begin(ctx, protocolsession.MessageReleaseLease, body)
	if err != nil {
		return err
	}
	defer func() { _ = client.rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	message, err := client.rpc.await(ctx, call)
	if err != nil {
		return err
	}
	if message.Kind() == protocolsession.MessageOperationError {
		return remoteOperationErrorFor(message, protocolsession.OperationScopeRevision)
	}
	if message.Kind() != protocolsession.MessageOperationComplete {
		return ErrOperationMissing
	}
	unsigned, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		return err
	}
	count, err := contentflow.DecodeOperationComplete(unsigned)
	if err != nil || count != 0 {
		return errors.Join(ErrOperationMissing, err)
	}
	return nil
}

func (client *receiverRevisionClient) renewLoop(state *remoteLeaseState) {
	for state.renewAfter > 0 {
		select {
		case <-state.ctx.Done():
			return
		case <-client.after(state.renewAfter):
		}
		ctx, cancel := context.WithTimeout(state.ctx, state.ttl)
		lease, err := client.renew(ctx, state.id)
		cancel()
		if err != nil {
			state.setError(err)
			return
		}
		state.ttl, state.renewAfter = lease.TTL, lease.RenewAfter
	}
}

func (client *receiverRevisionClient) renew(ctx context.Context, leaseID content.LeaseID) (contentflow.RemoteLease, error) {
	body, err := contentflow.EncodeLeaseRequest(leaseID)
	if err != nil {
		return contentflow.RemoteLease{}, err
	}
	call, err := client.rpc.begin(ctx, protocolsession.MessageRenewLease, body)
	if err != nil {
		return contentflow.RemoteLease{}, err
	}
	defer func() { _ = client.rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	message, err := client.rpc.await(ctx, call)
	if err != nil {
		return contentflow.RemoteLease{}, err
	}
	if message.Kind() == protocolsession.MessageOperationError {
		return contentflow.RemoteLease{}, remoteOperationErrorFor(message, protocolsession.OperationScopeRevision)
	}
	if message.Kind() != protocolsession.MessageLeaseResult {
		return contentflow.RemoteLease{}, ErrOperationMissing
	}
	unsigned, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		return contentflow.RemoteLease{}, err
	}
	return contentflow.DecodeLeaseResult(unsigned, leaseID)
}

func (client *receiverRevisionClient) leaseError(leaseID content.LeaseID) error {
	client.mu.Lock()
	state := client.leases[leaseID]
	client.mu.Unlock()
	if state == nil {
		return ErrRuntimeClosed
	}
	return state.Err()
}

func (client *receiverRevisionClient) stop() {
	if client == nil {
		return
	}
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return
	}
	client.closed = true
	states := make([]*remoteLeaseState, 0, len(client.leases))
	for _, state := range client.leases {
		states = append(states, state)
	}
	clear(client.leases)
	if client.cancel != nil {
		client.cancel()
	}
	client.mu.Unlock()
	for _, state := range states {
		state.close()
	}
}

func (client *receiverRevisionClient) close() {
	if client == nil {
		return
	}
	client.stop()
	client.work.Wait()
	client.mu.Lock()
	// Public closed handles remain callable, but no post-close path needs the RPC,
	// crypto opener, timer, or lifecycle graph they borrowed from the runtime.
	client.rpc = nil
	client.opener = nil
	client.after = nil
	client.ctx = nil
	client.cancel = nil
	client.leases = nil
	client.mu.Unlock()
}

type receiverBlockLane struct {
	identity  LaneIdentity
	rpc       *rpcClient
	assembler *contentflow.Assembler
	opener    RecordOpener
	revisions *receiverRevisionClient
}

func (lane *receiverBlockLane) FetchBlock(
	ctx context.Context,
	demand transfer.BlockDemand,
) (records.BlockRecord, error) {
	if err := lane.revisions.leaseError(demand.LeaseID); err != nil {
		return records.BlockRecord{}, err
	}
	request, err := contentflow.NewBlockRequest(demand.LeaseID, []uint64{demand.Index})
	if err != nil {
		return records.BlockRecord{}, err
	}
	body, err := contentflow.EncodeBlockRequest(request)
	if err != nil {
		return records.BlockRecord{}, err
	}
	call, err := lane.rpc.beginOn(ctx, &lane.identity, protocolsession.MessageRequestBlocks, body)
	if err != nil {
		if ctx.Err() == nil && lane.rpc.runtime.ctx.Err() == nil && requestProvenNotDelivered(err) {
			return records.BlockRecord{}, transfer.NewDemandNotAdmitted(err)
		}
		return records.BlockRecord{}, err
	}
	defer func() { _ = lane.rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	defer func() { _ = lane.assembler.CancelOperation(call.id) }()
	var record records.BlockRecord
	for {
		message, err := lane.rpc.await(ctx, call)
		if err != nil {
			return records.BlockRecord{}, err
		}
		switch message.Kind() {
		case protocolsession.MessageBlockFragment:
			result, err := lane.assembler.AcceptAuthenticated(message.Body())
			if err != nil {
				return records.BlockRecord{}, err
			}
			if result.Status == contentflow.RecordComplete {
				record, err = lane.opener.OpenBlock(demand.Descriptor, demand.Index, result.Object)
				if err != nil {
					return records.BlockRecord{}, err
				}
			}
		case protocolsession.MessageOperationError:
			return records.BlockRecord{}, remoteOperationErrorFor(message, protocolsession.OperationScopeBlock)
		case protocolsession.MessageOperationComplete:
			unsigned, err := protocolsession.SenderControlSemanticBody(message)
			if err != nil {
				return records.BlockRecord{}, err
			}
			count, err := contentflow.DecodeOperationComplete(unsigned)
			if err != nil || count != 1 || record.DataLength() == 0 {
				return records.BlockRecord{}, errors.Join(ErrOperationMissing, err)
			}
			_ = lane.assembler.CompleteOperation(call.id)
			return record, nil
		default:
			return records.BlockRecord{}, ErrOperationMissing
		}
	}
}
