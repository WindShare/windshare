package sessionruntime

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
)

// NewTransferJob binds one confirmed receive intent to a single transfer run.
// The materializer may reopen durable state, but the live protocol session can
// never derive OperationID from its per-run TransferJobID.
func (runtime *ReceiverRuntime) NewTransferJob(
	intent transfer.ReceiveIntent,
	jobID transfer.TransferJobID,
	materializer transfer.DirectTreeMaterializer,
	tracer transfer.TransferLifecycleTracer,
) (*transfer.TransferJob, error) {
	if runtime == nil || intent.IsZero() || jobID.IsZero() || materializer == nil {
		return nil, transfer.ErrInvalidTransferJob
	}
	descriptor := runtime.descriptor
	if intent.ShareInstance() != descriptor.ShareInstance() ||
		intent.SyntheticRoot() != descriptor.SyntheticRoot() {
		return nil, transfer.ErrInvalidTransferJob
	}
	dependencies := receiverTransferDependencies{runtime: runtime}
	return transfer.NewTransferJob(transfer.TransferJobConfig{
		ReceiveIntent: intent, JobID: jobID,
		ProtocolSessionID: runtime.ProtocolSessionID(), Tracer: tracer,
		Catalog: dependencies, Revisions: dependencies, Blocks: dependencies, Materializer: materializer,
	})
}

// ResolveOrdinaryOutputShape consumes only authenticated catalog metadata from
// this live session. The returned proof is construction-time state and cannot
// outlive the canonical ReceiveIntent assembled by its caller.
func (runtime *ReceiverRuntime) ResolveOrdinaryOutputShape(
	ctx context.Context,
	selection transfer.SelectionSpec,
	budget ordinaryoutput.ShapeProbeBudget,
	tracer ordinaryoutput.ShapeTracer,
) (ordinaryoutput.ShapeDecision, error) {
	if runtime == nil || runtime.runtimeCore == nil || runtime.catalog == nil {
		return ordinaryoutput.ShapeDecision{}, ErrRuntimeClosed
	}
	if ctx == nil {
		return ordinaryoutput.ShapeDecision{}, ordinaryoutput.ErrInvalidShapeResolution
	}
	input, err := selection.OrdinaryOutputSelection()
	if err != nil {
		return ordinaryoutput.ShapeDecision{}, err
	}
	operationContext, endAdmission, err := runtime.beginExternalAdmission(ctx)
	if err != nil {
		return ordinaryoutput.ShapeDecision{}, err
	}
	defer endAdmission()
	return ordinaryoutput.ResolveShape(
		operationContext,
		receiverTransferDependencies{runtime: runtime},
		input,
		budget,
		ordinaryoutput.BindShapeTracerToSession(runtime.ProtocolSessionID(), tracer),
	)
}

// receiverTransferDependencies is the semantic boundary between a live session
// and one transfer job. Dependency-specific close errors are useful while their
// owner is live, but once the runtime closes they all describe the same terminal
// fact: retrying another file cannot succeed on this authenticated session.
type receiverTransferDependencies struct {
	runtime *ReceiverRuntime
}

func (dependencies receiverTransferDependencies) OpenDirectoryPages(
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

func (dependencies receiverTransferDependencies) OpenRevision(
	ctx context.Context,
	file catalog.FileID,
) (transfer.OpenedRevision, error) {
	opened, err := dependencies.runtime.revisions.OpenRevision(ctx, file)
	return opened, dependencies.classifySource(ctx, err)
}

func (dependencies receiverTransferDependencies) ReleaseRevision(
	ctx context.Context,
	lease content.LeaseID,
) error {
	return dependencies.classifySource(ctx, dependencies.runtime.revisions.ReleaseRevision(ctx, lease))
}

func (dependencies receiverTransferDependencies) ReadRange(
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
		case receiverSessionTerminal(cause):
			return sessionTransportBoundaryError(cause)
		default:
			return dependencyBoundaryError(cause)
		}
	})
}

func (dependencies receiverTransferDependencies) classifySource(ctx context.Context, err error) error {
	return dependencies.classifyBoundary(ctx, err, func(cause error) error {
		switch {
		case errors.Is(cause, content.ErrRevisionDrift), errors.Is(cause, transfer.ErrBlockInvalidated):
			return sourceBoundaryError(transferfault.SourceRevisionInvalidated, cause)
		case errors.Is(cause, content.ErrRevisionStale), errors.Is(cause, content.ErrSourceDrift):
			return sourceBoundaryError(transferfault.SourceRevisionChanged, cause)
		case errors.Is(cause, content.ErrRevisionNotFound), errors.Is(cause, content.ErrRevisionUnreadable),
			errors.Is(cause, content.ErrUnsupportedStability):
			return sourceBoundaryError(transferfault.SourcePermanent, cause)
		case errors.Is(cause, content.ErrQuotaExceeded), errors.Is(cause, content.ErrLeaseExpired),
			errors.Is(cause, content.ErrInvalidLease):
			return sourceBoundaryError(transferfault.SourceUnavailable, cause)
		case errors.Is(cause, transfer.ErrBlockIdentity):
			return sessionProtocolBoundaryError(cause)
		case errors.Is(cause, transfer.ErrBrokerClosed), errors.Is(cause, transfer.ErrLaneClosed):
			return sessionTransportBoundaryError(cause)
		case receiverSessionTerminal(cause):
			return sessionTransportBoundaryError(cause)
		default:
			return dependencyBoundaryError(cause)
		}
	})
}

func receiverSessionTerminal(err error) bool {
	return errors.Is(err, protocolsession.ErrSessionTerminated) ||
		errors.Is(err, protocolsession.ErrPeerSessionTerminal) ||
		errors.Is(err, protocolsession.ErrWriterTerminal) ||
		errors.Is(err, protocolsession.ErrWriterStopped)
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
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var normalized *transferfault.BoundaryError
	if errors.As(err, &normalized) && normalized != nil && normalized.Fault().Valid() {
		return err
	}
	runtime := dependencies.runtime
	if errors.Is(err, ErrRuntimeClosed) || runtime == nil || runtime.runtimeCore == nil {
		return sessionTransportBoundaryError(err)
	}
	select {
	case <-runtime.ctx.Done():
		cause := runtime.Err()
		if cause == nil {
			cause = ErrRuntimeClosed
		}
		return sessionTransportBoundaryError(errors.Join(err, cause))
	default:
		return classify(err)
	}
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
		return ErrRuntimeConfig
	}
	return observe(ctx, progress)
}

type rpcCatalogTransport struct {
	rpc      *rpcClient
	progress CatalogScanProgressObserver
}

func (transport rpcCatalogTransport) FetchPage(ctx context.Context, request catalogflow.ListRequest) ([]byte, error) {
	body, err := catalogflow.EncodeListRequest(request)
	if err != nil {
		return nil, err
	}
	call, err := transport.rpc.begin(ctx, protocolsession.MessageListChildren, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transport.rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	var progressState catalogProgressState
	for {
		message, err := transport.rpc.await(ctx, call)
		if err != nil {
			return nil, err
		}
		switch message.Kind() {
		case protocolsession.MessageScanProgress:
			unsigned, err := protocolsession.SenderControlSemanticBody(message)
			if err != nil {
				return nil, sessionProtocolBoundaryError(err)
			}
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
			unsigned, err := protocolsession.SenderControlSemanticBody(message)
			if err != nil {
				return nil, err
			}
			return catalogflow.DecodeCatalogResult(unsigned)
		default:
			return nil, ErrOperationMissing
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
