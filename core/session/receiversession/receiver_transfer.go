package receiversession

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/revisionaccess"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

// NewTransferJob binds one confirmed receive intent to a single transfer run.
// The materializer may reopen durable state, but the live protocol session can
// never derive OperationID from its per-run TransferJobID.
func (runtime *Runtime) NewTransferJob(
	intent transfer.ReceiveIntent,
	jobID transfer.TransferJobID,
	materializer transfer.DirectTreeMaterializer,
	diag diagnostics.Sink,
) (*transfer.TransferJob, error) {
	if runtime == nil || intent.IsZero() || jobID.IsZero() || materializer == nil {
		return nil, transfer.ErrInvalidTransferJob
	}
	descriptor := runtime.descriptor
	if intent.ShareInstance() != descriptor.ShareInstance() ||
		intent.SyntheticRoot() != descriptor.SyntheticRoot() {
		return nil, transfer.ErrInvalidTransferJob
	}
	revisionWait, err := newReceiverRevisionWait(runtime)
	if err != nil {
		return nil, errors.Join(transfer.ErrInvalidTransferJob, err)
	}
	dependencies := receiverTransferDependencies{runtime: runtime}
	return transfer.NewTransferJob(transfer.TransferJobConfig{
		ReceiveIntent: intent, JobID: jobID,
		Session: runtime, Diagnostics: diag,
		Catalog: dependencies, Revisions: dependencies, Blocks: dependencies, Materializer: materializer,
		RevisionWait: revisionWait,
	})
}

func newReceiverRevisionWait(runtime *Runtime) (*revisionwait.Config, error) {
	if runtime == nil || runtime.core == nil || runtime.Lifetime() == nil {
		return nil, session.ErrRuntimeClosed
	}
	sessionID := runtime.ProtocolSessionID()
	if sessionID.IsZero() {
		return nil, revisionwait.ErrInvalidConfig
	}
	return &revisionwait.Config{
		WatchSession: revisionwait.WatchLifetime(sessionID, runtime.Lifetime()),
	}, nil
}

// ResolveOrdinaryOutputShape consumes only authenticated catalog metadata from
// this live session. The returned proof is construction-time state and cannot
// outlive the canonical ReceiveIntent assembled by its caller.
func (runtime *Runtime) ResolveOrdinaryOutputShape(
	ctx context.Context,
	selection transfer.SelectionSpec,
	budget ordinaryoutput.ShapeProbeBudget,
) (ordinaryoutput.ShapeDecision, error) {
	if runtime == nil || runtime.core == nil || runtime.catalog == nil {
		return ordinaryoutput.ShapeDecision{}, session.ErrRuntimeClosed
	}
	if ctx == nil {
		return ordinaryoutput.ShapeDecision{}, ordinaryoutput.ErrInvalidShapeResolution
	}
	input, err := selection.OrdinaryOutputSelection()
	if err != nil {
		return ordinaryoutput.ShapeDecision{}, err
	}
	operationContext, endAdmission, err := runtime.core.BeginExternalAdmission(ctx)
	if err != nil {
		return ordinaryoutput.ShapeDecision{}, err
	}
	defer endAdmission()
	return ordinaryoutput.ResolveShape(
		operationContext,
		prefetchRecordingCatalog{dependencies: receiverTransferDependencies{runtime: runtime}},
		input,
		budget,
	)
}

// TransferDependencies exposes protocol operations without giving a transfer owner
// access to the runtime's crypto, grants, or lease registry.
func (runtime *Runtime) TransferDependencies() (TransferDependencies, error) {
	_, err := newReceiverRevisionWait(runtime)
	return receiverTransferDependencies{runtime: runtime}, err
}

type TransferDependencies = receiverTransferDependencies

// PathsExhausted reports the winning lifecycle authority, never an error-text
// classification. Protocol failures and caller cleanup cannot authorize recovery.
func (runtime *Runtime) PathsExhausted() bool {
	if runtime == nil || runtime.core == nil {
		return false
	}
	return runtime.core.PathsExhausted()
}

// PathRecoveryFailure preserves the network root after an external replacement
// budget or handshake fails. Child timeout diagnostics are not caller cancellation.
func (runtime *Runtime) PathRecoveryFailure(cause error) error {
	if runtime.PathsExhausted() {
		return sessionTransportBoundaryError(cause)
	}
	return dependencyBoundaryError(cause)
}

// AwaitPathRetirement joins the physical lane owners before inspecting their
// winning termination. A request can observe no usable lane while its pump is
// still retiring; that scheduling race must not discard valid output progress.
func (runtime *Runtime) AwaitPathRetirement(ctx context.Context) bool {
	if runtime == nil || runtime.core == nil || ctx == nil {
		return false
	}
	if runtime.PathsExhausted() {
		return true
	}
	if runtime.core.HasUsableLanes() {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-runtime.Done():
		return runtime.PathsExhausted()
	}
}

// receiverTransferDependencies is the semantic boundary between a live session
// and one transfer job. Dependency-specific close errors are useful while their
// owner is live, but once the runtime closes they all describe the same terminal
// fact: retrying another file cannot succeed on this authenticated session.
type receiverTransferDependencies struct {
	runtime *Runtime
}

func (dependencies receiverTransferDependencies) OpenDirectoryPages(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if dependencies.runtime != nil && dependencies.runtime.core != nil &&
		dependencies.runtime.Lifetime().Err() == nil {
		if pages, ok := dependencies.runtime.takePrefetchedDirectory(directory); ok {
			return &prefetchedDirectoryPageCursor{
				pages:        pages,
				dependencies: dependencies,
			}, nil
		}
	}
	return dependencies.OpenDirectoryPagesLive(ctx, directory)
}

// OpenDirectoryPagesLive bypasses any shape-resolution prefetch cache and opens
// a fresh forward-only cursor directly on the live catalog client.
func (dependencies receiverTransferDependencies) OpenDirectoryPagesLive(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	cursor, err := dependencies.runtime.catalog.OpenDirectoryPages(ctx, directory)
	if err != nil {
		return nil, dependencies.classifyCatalog(ctx, err)
	}
	if cursor == nil {
		return nil, dependencyBoundaryError(transfer.ErrCatalogCursorContract)
	}
	return receiverDirectoryPageCursor{cursor: cursor, dependencies: dependencies}, nil
}

type receiverDirectoryPageCursor struct {
	cursor       catalog.DirectoryPageCursor
	dependencies receiverTransferDependencies
}

func (cursor receiverDirectoryPageCursor) Next(
	ctx context.Context,
) (catalog.CatalogPage, bool, error) {
	page, ok, err := cursor.cursor.Next(ctx)
	return page, ok, cursor.dependencies.classifyCatalog(ctx, err)
}

func (cursor receiverDirectoryPageCursor) Close() error {
	return cursor.dependencies.classifyCatalog(context.Background(), cursor.cursor.Close())
}

func (dependencies receiverTransferDependencies) AcquireDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	snapshot, release, err := dependencies.runtime.catalog.AcquireDirectory(ctx, directory)
	return snapshot, release, dependencies.classifyCatalog(ctx, err)
}

func (dependencies receiverLeaseDependencies) OpenLease(
	ctx context.Context,
	file catalog.FileID,
) (revisionaccess.Lease, error) {
	opened, err := dependencies.runtime.revisions.OpenRevision(ctx, file)
	// Direct type assertion rejects diagnostic wrappers so only an authenticated
	// remote response can produce capacity retry authority.
	if remote, ok := err.(*RemoteRevisionError); ok && remote != nil { //nolint:errorlint // Intentionally rejects wrapped errors.
		if dependencies.emitCapacitySignal {
			if capacity, recognized := dependencies.authenticatedCapacitySignal(remote); recognized {
				return opened, capacity
			}
		}
		err = remoteRevisionFailureError(remote)
	}
	return opened, dependencies.classifyAccess(ctx, err)
}

func (dependencies receiverTransferDependencies) authenticatedCapacitySignal(
	remote *RemoteRevisionError,
) (error, bool) {
	if remote == nil {
		return nil, false
	}
	failure := remote.Failure()
	if failure.Code != contentflow.RevisionCodeQuota || !failure.Retryable || failure.RetryAfter == 0 {
		return nil, false
	}
	runtime := dependencies.runtime
	if runtime == nil || remote.ProtocolSessionID().IsZero() || remote.OperationID().IsZero() ||
		!remote.ProtocolSessionID().Equal(runtime.ProtocolSessionID()) {
		return dependencyBoundaryError(errors.Join(transfer.ErrInvalidTransferJob, remote)), true
	}
	signal, err := revisionwait.NewCapacitySignal(
		failure.RetryAfter, remote.ProtocolSessionID(), remote.OperationID(),
	)
	if err != nil {
		return sessionProtocolBoundaryError(errors.Join(protocolsession.ErrInvalidOperationFailure, remote, err)), true
	}
	return signal, true
}

func (dependencies receiverLeaseDependencies) ReleaseLease(
	ctx context.Context,
	lease content.LeaseID,
) error {
	return dependencies.classifySource(ctx, dependencies.runtime.revisions.ReleaseRevision(ctx, lease))
}

func (dependencies receiverLeaseDependencies) ReadLeaseRange(
	ctx context.Context,
	lease content.LeaseID,
	descriptor content.FileRevisionDescriptor,
	requested content.Range,
	sink transfer.RangeSink,
) error {
	return dependencies.classifySource(
		ctx,
		dependencies.runtime.broker.ReadRange(ctx, lease, descriptor, requested, sink),
	)
}

func (dependencies receiverTransferDependencies) classifyCatalog(ctx context.Context, err error) error {
	return dependencies.classifyBoundary(ctx, err, func(cause error) error {
		var directoryFailure catalogflow.DirectoryFailure
		switch {
		case errors.As(cause, &directoryFailure):
			return catalogDirectoryBoundaryError(cause)
		case errors.Is(cause, catalog.ErrDirectoryStale):
			value, _ := transferfault.NewCatalog(
				transferfault.ScopeDirectoryLocal, transferfault.CatalogDirectoryStale,
			)
			return transferfault.Wrap(value, cause)
		case transferfault.IsSessionTerminal(cause):
			return sessionTransportBoundaryError(cause)
		default:
			return dependencyBoundaryError(cause)
		}
	})
}

func (dependencies receiverTransferDependencies) classifySource(ctx context.Context, err error) error {
	return dependencies.classifyBoundary(ctx, err, func(cause error) error {
		if errors.Is(cause, transfer.ErrBlockInvalidated) {
			return sourceBoundaryError(transferfault.SourceRevisionInvalidated, cause)
		}
		if value, known := transferfault.ClassifyContent(cause); known {
			return transferfault.Wrap(value, cause)
		}
		switch {
		case errors.Is(cause, transfer.ErrBlockIdentity):
			return sessionProtocolBoundaryError(cause)
		case errors.Is(cause, transfer.ErrBrokerClosed), errors.Is(cause, transferlanes.ErrClosed),
			transferfault.IsSessionTerminal(cause):
			return sessionTransportBoundaryError(cause)
		default:
			return dependencyBoundaryError(cause)
		}
	})
}

func (dependencies receiverTransferDependencies) classifyBoundary(
	ctx context.Context,
	err error,
	classify func(error) error,
) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return err
	}
	runtime := dependencies.runtime
	var normalized *transferfault.BoundaryError
	if errors.As(err, &normalized) && normalized != nil && normalized.Fault().Valid() {
		// A local file/output fault keeps its own scope even when another path
		// simultaneously retires. Only session cancellation inherits its owner.
		if normalized.Fault().Domain() != transferfault.DomainSession ||
			runtime == nil || runtime.core == nil || runtime.Lifetime().Err() == nil {
			return err
		}
		return receiverTerminalBoundaryError(runtime, err)
	}
	if runtime != nil && runtime.core != nil && runtime.Lifetime().Err() != nil {
		return receiverTerminalBoundaryError(runtime, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, session.ErrRuntimeClosed) || runtime == nil || runtime.core == nil {
		return sessionTransportBoundaryError(err)
	}
	return classify(err)
}

func receiverTerminalBoundaryError(runtime *Runtime, err error) error {
	cause := runtime.Err()
	if cause == nil {
		cause = session.ErrRuntimeClosed
	}
	var boundary *transferfault.BoundaryError
	if errors.As(cause, &boundary) && boundary != nil &&
		boundary.Fault().Domain() == transferfault.DomainSession && boundary.Fault().Scope() == transferfault.ScopeSessionTerminal {
		return transferfault.Wrap(boundary.Fault(), errors.Join(err, cause))
	}
	return sessionTransportBoundaryError(errors.Join(err, cause))
}

type CatalogScanProgress struct {
	DirectoryID       catalog.DirectoryID
	AttemptID         catalog.ScanAttemptID
	DiscoveredEntries uint64
}

type CatalogScanProgressObserver interface {
	ObserveCatalogScanProgress(context.Context, CatalogScanProgress) error
}

type CatalogScanProgressObserverFunc func(context.Context, CatalogScanProgress) error

func (observe CatalogScanProgressObserverFunc) ObserveCatalogScanProgress(
	ctx context.Context,
	progress CatalogScanProgress,
) error {
	if observe == nil {
		return session.ErrRuntimeConfig
	}
	return observe(ctx, progress)
}

type rpcCatalogTransport struct {
	rpc      requestClient
	progress CatalogScanProgressObserver
}

func (transport rpcCatalogTransport) FetchPage(ctx context.Context, request catalogflow.ListRequest) ([]byte, error) {
	body, err := catalogflow.EncodeListRequest(request)
	if err != nil {
		return nil, err
	}
	return runControlRequest(ctx, transport.rpc, protocolsession.MessageListChildren, body, func(call *sessioncore.Call) ([]byte, error) {
		return transport.receivePage(ctx, request, call)
	})
}

func (transport rpcCatalogTransport) receivePage(ctx context.Context, request catalogflow.ListRequest, call *sessioncore.Call) ([]byte, error) {
	var progressState catalogProgressState
	for {
		message, err := transport.rpc.Await(ctx, call)
		if err != nil {
			return nil, err
		}
		switch message.Kind() {
		case protocolsession.MessageScanProgress:
			unsigned := message.Body()
			progress, err := protocolsession.DecodeScanProgress(unsigned)
			notify, progressErr := progressState.observe(progress)
			if err != nil || progressErr != nil {
				return nil, sessionProtocolBoundaryError(errors.Join(ErrScanProgress, err, progressErr))
			}
			if !notify {
				continue
			}
			if transport.progress != nil {
				if err := transport.progress.ObserveCatalogScanProgress(ctx, CatalogScanProgress{
					DirectoryID: request.DirectoryID(), AttemptID: progress.AttemptID,
					DiscoveredEntries: progress.DiscoveredEntries,
				}); err != nil {
					return nil, err
				}
			}
			continue
		case protocolsession.MessageOperationError:
			return nil, remoteDirectoryOperationError(message)
		case protocolsession.MessageCatalogResult:
			unsigned := message.Body()
			return catalogflow.DecodeCatalogResult(unsigned)
		default:
			return nil, session.ErrOperationMissing
		}
	}
}

type catalogProgressState struct {
	attempt    catalog.ScanAttemptID
	discovered uint64
	seen       bool
}

func (state *catalogProgressState) observe(progress protocolsession.ScanProgress) (bool, error) {
	if state.seen && (progress.AttemptID != state.attempt || progress.DiscoveredEntries < state.discovered) {
		return false, ErrScanProgress
	}
	if state.seen && progress.DiscoveredEntries == state.discovered {
		return false, nil
	}
	state.attempt, state.discovered, state.seen = progress.AttemptID, progress.DiscoveredEntries, true
	return true, nil
}

func (runtime *Runtime) bindRevisionAccess() error {
	if _, err := newReceiverRevisionWait(runtime); err != nil {
		return err
	}
	source := receiverLeaseDependencies{
		receiverTransferDependencies: receiverTransferDependencies{runtime: runtime},
		emitCapacitySignal:           true,
	}
	runtime.access = revisionaccess.New(runtime.Lifetime(), source)
	return runtime.core.AddFinalizer(runtime.releaseOwnedResources)
}

// The local handle owns one file's wire lease across output attempts. Each
// range request must still match the descriptor admitted by its own writer.
type receiverLeaseDependencies struct {
	receiverTransferDependencies
	emitCapacitySignal bool
}

func (dependencies receiverTransferDependencies) OpenRevision(ctx context.Context, file catalog.FileID) (transfer.OpenedRevision, error) {
	opened, err := dependencies.runtime.access.OpenRevision(ctx, file)
	return opened, dependencies.classifyAccess(ctx, err)
}

func (dependencies receiverTransferDependencies) ReleaseRevision(ctx context.Context, handle transfer.RevisionHandle) error {
	return dependencies.classifyAccess(ctx, dependencies.runtime.access.ReleaseRevision(ctx, handle))
}

func (dependencies receiverTransferDependencies) ObserveRemainingWork(snapshot func() transferlanes.RemainingWork) func() {
	return dependencies.runtime.broker.ObserveRemainingWork(snapshot)
}

func (dependencies receiverTransferDependencies) RecordOutput(bytes uint64) {
	dependencies.runtime.laneSet.RecordOutput(bytes)
}

func (dependencies receiverTransferDependencies) ReadRange(ctx context.Context, handle transfer.RevisionHandle, descriptor content.FileRevisionDescriptor, requested content.Range, sink transfer.RangeSink) error {
	err := dependencies.runtime.access.ReadRange(ctx, handle, descriptor, requested, sink)
	if _, reopened := errors.AsType[*content.RevisionReopenedError](err); reopened && ctx.Err() == nil && dependencies.runtime.Lifetime().Err() == nil {
		return err
	}
	if errors.Is(err, revisionaccess.ErrClosed) {
		err = errors.Join(transfer.ErrBrokerClosed, err)
	}
	return dependencies.classifyAccess(ctx, err)
}

func (dependencies receiverTransferDependencies) classifyAccess(ctx context.Context, err error) error {
	if _, capacity := revisionwait.MatchCapacitySignal(err); capacity {
		return err
	}
	if ctx.Err() == nil && (errors.Is(err, revisionaccess.ErrClosed) ||
		errors.Is(err, context.Canceled) && dependencies.runtime.Lifetime().Err() != nil) {
		return receiverTerminalBoundaryError(dependencies.runtime, errors.Join(session.ErrRuntimeClosed, err))
	}
	return dependencies.classifySource(ctx, err)
}

const maximumBlockObjectOverhead = records.MaxBlockRecordObjectBytes - catalog.MaxChunkSize

func (operation *receiverBlockOperation) complete(message protocolsession.Message, record records.BlockRecord) (records.BlockRecord, error) {
	unsigned := message.Body()
	count, err := contentflow.DecodeOperationComplete(unsigned)
	if err != nil || count != 1 {
		return operation.rejectCompletion(errors.Join(session.ErrOperationMissing, err))
	}
	if record.DataLength() == 0 {
		carrier := operation.call.LastResponseLane()
		if carrier.Valid(true) && carrier != operation.lane.identity {
			// Transport admission does not prove delivery. A final migrating to
			// another authenticated carrier can overtake or outlive old fragments.
			// Retire this exact operation before requesting the missing block anew.
			operation.call.RecordTraceCause(sessionobs.ProtocolOperationCauseLaneUnavailable)
			return operation.reassignAfterRetirement(sessioncore.ErrLaneUnavailable, contentflow.CancelReasonOutputAbort)
		}
		return operation.rejectCompletion(session.ErrOperationMissing)
	}
	return record, nil
}

func (operation *receiverBlockOperation) rejectCompletion(err error) (records.BlockRecord, error) {
	operation.call.RecordTraceCause(sessionobs.ProtocolOperationCauseProtocolFailure)
	return records.BlockRecord{}, err
}

type reservedBlockLane struct {
	lane     *receiverBlockLane
	capacity *contentflow.AssemblyCapacity
}

func (lane *receiverBlockLane) TryReserveBlock(demand transferlanes.Demand) (transferlanes.BlockReservation, <-chan struct{}, error) {
	if err := lane.revisions.leaseError(demand.LeaseID); err != nil {
		return transferlanes.BlockReservation{}, nil, err
	}
	bytes, err := demand.Descriptor.Geometry().BlockPlainLength(demand.Index)
	if err != nil {
		return transferlanes.BlockReservation{}, nil, err
	}
	capacity, changed, err := lane.assembler.TryReserve(uint64(bytes) + maximumBlockObjectOverhead)
	if capacity == nil {
		return transferlanes.BlockReservation{}, changed, err
	}
	return transferlanes.BlockReservation{Source: reservedBlockLane{lane, capacity}, Release: capacity.Release}, nil, nil
}

func (reserved reservedBlockLane) FetchBlock(ctx context.Context, demand transferlanes.Demand, received func(uint64)) (records.BlockRecord, error) {
	return reserved.lane.fetchReservedBlock(ctx, demand, received, reserved.capacity)
}

func (lane *receiverBlockLane) FetchBlock(ctx context.Context, demand transferlanes.Demand, received func(uint64)) (records.BlockRecord, error) {
	for {
		if err := ctx.Err(); err != nil {
			return records.BlockRecord{}, err
		}
		reserved, changed, err := lane.TryReserveBlock(demand)
		if err != nil {
			return records.BlockRecord{}, err
		}
		if reserved.Source != nil {
			defer reserved.Release()
			return reserved.Source.FetchBlock(ctx, demand, received)
		}
		select {
		case <-ctx.Done():
			return records.BlockRecord{}, ctx.Err()
		case <-changed:
		}
	}
}

var ErrScanProgress = errors.New("session runtime scan progress changed identity or regressed")
