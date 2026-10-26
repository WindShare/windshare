package receiversession

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/revisionaccess"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"sync"
	"time"
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
	rpc       requestClient
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
	rpc requestClient,
	opener RecordOpener,
	chunkSize uint32,
	after func(time.Duration) <-chan time.Time,
) *receiverRevisionClient {
	ctx, cancel := context.WithCancel(rpc.Runtime().Lifetime())
	return &receiverRevisionClient{
		rpc: rpc, opener: opener, chunkSize: chunkSize, after: after,
		ctx: ctx, cancel: cancel, leases: make(map[content.LeaseID]*remoteLeaseState),
	}
}

func (client *receiverRevisionClient) OpenRevision(
	ctx context.Context,
	fileID catalog.FileID,
) (revisionaccess.Lease, error) {
	// RecordOpener has no context and may execute application-controlled crypto.
	// Runtime admission is therefore the ownership boundary that keeps its key
	// resources alive even when the session is cancelled from inside the callback.
	ctx, rpc, endAdmission, err := client.beginExternalOperation(ctx)
	if err != nil {
		return revisionaccess.Lease{}, err
	}
	defer endAdmission()
	emptyRanges, err := content.NewRangeSet(nil)
	if err != nil {
		return revisionaccess.Lease{}, err
	}
	request, err := contentflow.NewOpenRequest([]contentflow.OpenItem{{FileID: fileID, InitialRanges: emptyRanges}})
	if err != nil {
		return revisionaccess.Lease{}, err
	}
	body, err := contentflow.EncodeOpenRequest(request)
	if err != nil {
		return revisionaccess.Lease{}, err
	}
	call, err := rpc.Begin(ctx, protocolsession.MessageOpenRevisions, body)
	if err != nil {
		return revisionaccess.Lease{}, err
	}
	defer func() { _ = rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	message, err := rpc.Await(ctx, call)
	if err != nil {
		return revisionaccess.Lease{}, err
	}
	if message.Kind() == protocolsession.MessageOperationError {
		return revisionaccess.Lease{}, remoteRevisionOperationError(message)
	}
	if message.Kind() != protocolsession.MessageOpenResults {
		return revisionaccess.Lease{}, sessioncontract.ErrOperationMissing
	}
	unsigned, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		return revisionaccess.Lease{}, err
	}
	results, err := contentflow.DecodeOpenResults(unsigned, []catalog.FileID{fileID})
	if err != nil || len(results) != 1 {
		return revisionaccess.Lease{}, errors.Join(sessioncontract.ErrOperationMissing, err)
	}
	if results[0].Failure != nil {
		return revisionaccess.Lease{}, &RemoteRevisionError{
			failure: *results[0].Failure, protocolSession: rpc.Runtime().ProtocolSessionID(),
			protocolOperation: call.ID(),
		}
	}
	if lifecycleErr := client.operationLifecycleError(ctx); lifecycleErr != nil {
		return revisionaccess.Lease{}, client.compensateRemoteLease(ctx, results[0].Lease.ID, lifecycleErr)
	}
	descriptor, err := client.opener.OpenRevision(fileID, client.chunkSize, results[0].RevisionObject)
	if err != nil {
		return revisionaccess.Lease{}, client.compensateRemoteLease(ctx, results[0].Lease.ID, err)
	}
	if lifecycleErr := client.operationLifecycleError(ctx); lifecycleErr != nil {
		return revisionaccess.Lease{}, client.compensateRemoteLease(ctx, results[0].Lease.ID, lifecycleErr)
	}
	opened, err := revisionaccess.NewLease(results[0].Lease.ID, descriptor)
	if err != nil {
		return revisionaccess.Lease{}, client.compensateRemoteLease(ctx, results[0].Lease.ID, err)
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
		return revisionaccess.Lease{}, client.compensateRemoteLease(ctx, state.id, sessioncontract.ErrRuntimeClosed)
	}
	if client.leases[state.id] != nil {
		client.mu.Unlock()
		state.close()
		return revisionaccess.Lease{}, client.failRemoteLeaseCollision()
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
) (context.Context, requestClient, func(), error) {
	if client == nil {
		return nil, nil, nil, sessioncontract.ErrRuntimeClosed
	}
	client.mu.Lock()
	if client.closed || client.rpc == nil {
		client.mu.Unlock()
		return nil, nil, nil, sessioncontract.ErrRuntimeClosed
	}
	rpc := client.rpc
	client.mu.Unlock()
	operationContext, endAdmission, err := rpc.Runtime().BeginExternalAdmission(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	client.mu.Lock()
	closed := client.closed || client.rpc != rpc
	client.mu.Unlock()
	if closed {
		endAdmission()
		return nil, nil, nil, sessioncontract.ErrRuntimeClosed
	}
	return operationContext, rpc, endAdmission, nil
}

func (client *receiverRevisionClient) operationLifecycleError(ctx context.Context) error {
	client.mu.Lock()
	closed := client.closed
	client.mu.Unlock()
	if closed || client.rpc.Runtime().Lifetime().Err() != nil {
		return errors.Join(sessioncontract.ErrRuntimeClosed, ctx.Err())
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
	runtime := client.rpc.Runtime()
	runtime.RecordError(ErrRemoteLeaseCollision)
	runtime.FailProtocol(runtime.Err())
	return ErrRemoteLeaseCollision
}

// RemoteRevisionError retains the authenticated OPEN_RESULTS diagnostic without
// letting observers mutate the semantic authority already assigned to the job.
type RemoteRevisionError struct {
	failure           contentflow.RevisionFailure
	protocolSession   protocolsession.ProtocolSessionID
	protocolOperation protocolsession.OperationID
}

func (err *RemoteRevisionError) Error() string { return "sender could not open the file revision" }

func (err *RemoteRevisionError) Failure() contentflow.RevisionFailure {
	return err.failure
}

func (err *RemoteRevisionError) ProtocolSessionID() protocolsession.ProtocolSessionID {
	if err == nil {
		return protocolsession.ProtocolSessionID{}
	}
	return err.protocolSession
}

func (err *RemoteRevisionError) OperationID() protocolsession.OperationID {
	if err == nil {
		return protocolsession.OperationID{}
	}
	return err.protocolOperation
}

func (client *receiverRevisionClient) ReleaseRevision(ctx context.Context, leaseID content.LeaseID) error {
	ctx, _, endAdmission, err := client.beginExternalOperation(ctx)
	if err != nil {
		return err
	}
	defer endAdmission()
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return sessioncontract.ErrRuntimeClosed
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
	call, err := client.rpc.Begin(ctx, protocolsession.MessageReleaseLease, body)
	if err != nil {
		return err
	}
	defer func() { _ = client.rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	message, err := client.rpc.Await(ctx, call)
	if err != nil {
		return err
	}
	if message.Kind() == protocolsession.MessageOperationError {
		return remoteRevisionOperationError(message)
	}
	if message.Kind() != protocolsession.MessageOperationComplete {
		return sessioncontract.ErrOperationMissing
	}
	unsigned, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		return err
	}
	count, err := contentflow.DecodeOperationComplete(unsigned)
	if err != nil || count != 0 {
		return errors.Join(sessioncontract.ErrOperationMissing, err)
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
	call, err := client.rpc.Begin(ctx, protocolsession.MessageRenewLease, body)
	if err != nil {
		return contentflow.RemoteLease{}, err
	}
	defer func() { _ = client.rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	message, err := client.rpc.Await(ctx, call)
	if err != nil {
		return contentflow.RemoteLease{}, err
	}
	if message.Kind() == protocolsession.MessageOperationError {
		return contentflow.RemoteLease{}, remoteRevisionOperationError(message)
	}
	if message.Kind() != protocolsession.MessageLeaseResult {
		return contentflow.RemoteLease{}, sessioncontract.ErrOperationMissing
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
		return sessioncontract.ErrRuntimeClosed
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
	identity                  sessioncontract.LaneIdentity
	rpc                       requestClient
	assembler                 *contentflow.Assembler
	opener                    RecordOpener
	revisions                 *receiverRevisionClient
	fragmentInactivityTimeout time.Duration
	responses                 contentflow.BlockResponseQueue
}

type receiverBlockOperation struct {
	capacity *contentflow.AssemblyCapacity
	lane     *receiverBlockLane
	call     *sessioncore.Call
	retired  bool
	terminal *sessioncore.TerminalOwner
}

func (lane *receiverBlockLane) fetchReservedBlock(
	ctx context.Context,
	demand transferlanes.Demand, received func(uint64),
	capacity *contentflow.AssemblyCapacity,
) (record records.BlockRecord, err error) {
	operation, err := lane.beginBlockOperation(ctx, demand)
	if err != nil {
		return records.BlockRecord{}, err
	}
	defer func() { err = operation.finish(ctx, err) }()
	operation.capacity = capacity
	return operation.receive(ctx, demand, received)
}

func (lane *receiverBlockLane) beginBlockOperation(
	ctx context.Context,
	demand transferlanes.Demand,
) (*receiverBlockOperation, error) {
	if err := lane.revisions.leaseError(demand.LeaseID); err != nil {
		return nil, err
	}
	request, err := contentflow.NewBlockRequest(demand.LeaseID, []uint64{demand.Index})
	if err != nil {
		return nil, err
	}
	body, err := contentflow.EncodeBlockRequest(request)
	if err != nil {
		return nil, err
	}
	call, message, err := lane.rpc.PrepareCall(ctx, protocolsession.MessageRequestBlocks, body)
	if err != nil {
		return nil, err
	}
	operation := &receiverBlockOperation{lane: lane, call: call, terminal: call.ClaimProtocolTermination()}
	if err := lane.rpc.BeginCall(ctx, &lane.identity, call, message); err != nil {
		operation.finishProtocolTrace(ctx, sessionobs.ProtocolOperationReceiverFailed)
		if ctx.Err() == nil && lane.rpc.Runtime().Lifetime().Err() == nil && sessioncore.RequestProvenNotDelivered(err) {
			return nil, transferlanes.NewDemandNotAdmitted(err)
		}
		return nil, err
	}
	return operation, nil
}

func (operation *receiverBlockOperation) finish(ctx context.Context, receiveErr error) error {
	operation.call.RecordTraceFailure(receiveErr)
	reason := contentflow.CancelReasonOutputAbort
	if transferlanes.AttemptSuperseded(ctx) {
		reason = contentflow.CancelReasonSuperseded
	}
	cleanupErr := operation.retire(reason)
	stage := sessionobs.ProtocolOperationReceiverCompleted
	if receiveErr != nil || cleanupErr != nil {
		stage = sessionobs.ProtocolOperationReceiverFailed
	}
	operation.finishProtocolTrace(ctx, stage)
	if cleanupErr != nil {
		return errors.Join(receiveErr, cleanupErr)
	}
	return receiveErr
}

func (operation *receiverBlockOperation) finishProtocolTrace(ctx context.Context, stage sessionobs.ProtocolOperationStage) {
	outcome := sessioncore.OperationOutcome{Stage: stage}
	if stage != sessionobs.ProtocolOperationReceiverCompleted && transferlanes.AttemptSuperseded(ctx) {
		outcome = sessioncore.OperationOutcome{Stage: sessionobs.ProtocolOperationReceiverEnded, Cause: sessionobs.ProtocolOperationCauseSuperseded}
	}
	operation.terminal.Finish(operation.lane.rpc.Runtime(), outcome)
}

func (operation *receiverBlockOperation) retire(reason contentflow.CancelReason) error {
	if operation.retired {
		return nil
	}
	operation.retired = true
	// The local reassembly tombstone is committed before the RPC sink is
	// removed, so late fragments cannot alias a replacement lane operation.
	assemblyErr := operation.lane.assembler.CancelOperation(operation.call.ID())
	operation.call.RecordTraceCleanup(assemblyErr)
	return errors.Join(assemblyErr, operation.lane.rpc.CancelAndEnd(operation.call, reason))
}

func (operation *receiverBlockOperation) receive(
	ctx context.Context,
	demand transferlanes.Demand,
	received func(uint64),
) (records.BlockRecord, error) {
	wait := operation.lane.responses.Begin(ctx, operation.lane.blockFragmentInactivityTimeout())
	defer wait.Close()
	var record records.BlockRecord
	for {
		wait.Resume()
		message, err := operation.lane.awaitBlockMessage(wait.Context(), operation.call)
		wait.Suspend()
		if err != nil {
			return operation.receiveFailure(err)
		}
		switch message.Kind() {
		case protocolsession.MessageBlockFragment:
			next, progressed, err := operation.acceptBlockFragment(demand, message, record, received)
			if err != nil {
				// Decoding has no cancelable wait. Even a lifecycle-shaped opener
				// error is a validation fault, regardless of a concurrent race winner.
				operation.call.RecordTraceCause(sessionobs.ProtocolOperationCauseProtocolFailure)
				return operation.receiveFailure(err)
			}
			record = next
			if progressed {
				wait.Progress()
			}
		case protocolsession.MessageOperationError:
			return records.BlockRecord{}, blockRequestOperationError(message)
		case protocolsession.MessageOperationComplete:
			return operation.complete(message, record)
		default:
			operation.call.RecordTraceCause(sessionobs.ProtocolOperationCauseProtocolFailure)
			return records.BlockRecord{}, sessioncontract.ErrOperationMissing
		}
	}
}

func (operation *receiverBlockOperation) receiveFailure(cause error) (records.BlockRecord, error) {
	if !errors.Is(cause, contentflow.ErrFragmentInactivity) && !errors.Is(cause, contentflow.ErrBlockResponseInactivity) {
		return records.BlockRecord{}, cause
	}
	return operation.reassignAfterRetirement(cause, contentflow.CancelReasonTimeout)
}

func (operation *receiverBlockOperation) reassignAfterRetirement(cause error, reason contentflow.CancelReason) (records.BlockRecord, error) {
	boundaryErr := blockTransportBoundary(cause)
	if retireErr := operation.retire(reason); retireErr != nil {
		return records.BlockRecord{}, errors.Join(boundaryErr, retireErr)
	}
	return records.BlockRecord{}, transferlanes.NewDemandReassignableAfterRetirement(boundaryErr)
}

func (operation *receiverBlockOperation) acceptBlockFragment(
	demand transferlanes.Demand,
	message protocolsession.Message,
	current records.BlockRecord,
	received func(uint64),
) (records.BlockRecord, bool, error) {
	result, err := operation.capacity.AcceptAuthenticated(message.Body())
	if err != nil {
		return records.BlockRecord{}, false, err
	}
	if received != nil && result.ReceivedBytes > 0 {
		received(result.ReceivedBytes)
	}
	progressed := result.Status == contentflow.FragmentAccepted || result.Status == contentflow.RecordComplete
	if result.Status != contentflow.RecordComplete {
		return current, progressed, nil
	}
	record, err := operation.lane.opener.OpenBlock(demand.Descriptor, demand.Index, result.Object)
	return record, progressed, err
}

func (lane *receiverBlockLane) blockFragmentInactivityTimeout() time.Duration {
	if lane.fragmentInactivityTimeout > 0 {
		return lane.fragmentInactivityTimeout
	}
	return contentflow.FragmentInactivityTimeout
}

func (lane *receiverBlockLane) awaitBlockMessage(
	ctx context.Context,
	call *sessioncore.Call,
) (protocolsession.Message, error) {
	var message protocolsession.Message
	err := ctx.Err()
	if err == nil {
		message, err = lane.rpc.AwaitCall(ctx, call, false)
	}
	if timeout, ok := errors.AsType[*contentflow.BlockWaitTimeout](context.Cause(ctx)); ok {
		// The progress clock cancels a single wait context only on real expiry,
		// so extending queue allowance never records a spurious RPC failure.
		call.RecordTraceFailure(timeout)
		return protocolsession.Message{}, timeout
	}
	return message, err
}

func blockTransportBoundary(cause error) error {
	value, err := transferfault.NewSession(transferfault.ScopeOutputPause, transferfault.SessionTransport)
	if err != nil {
		return errors.Join(cause, err)
	}
	return transferfault.Wrap(value, cause)
}
