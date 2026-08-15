package outputruntime

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var (
	ErrNativeResumeBusy             = resumeauthority.ErrBusy
	ErrNativeResumeOwnershipUnknown = errors.New("osfs: resume state ownership is unknown")
)

// NativeResumeRepository binds a destination afresh for each page or exact
// operation lease. The display path selects the root; durable authority comes
// only from the destination identity and ordinary-v1 records reopened below it.
type NativeResumeRepository struct {
	rootPath        string
	platformFactory PlatformFactory
}

func NewNativeResumeRepository(
	rootPath string,
	platformFactory PlatformFactory,
) (*NativeResumeRepository, error) {
	if rootPath == "" || !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath ||
		platformFactory == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return &NativeResumeRepository{rootPath: rootPath, platformFactory: platformFactory}, nil
}

func (repository *NativeResumeRepository) Page(
	ctx context.Context,
	cursor resumeauthority.PageCursor,
	maximum int,
) (result resumeauthority.Page, resultErr error) {
	if repository == nil || ctx == nil || repository.platformFactory == nil {
		return resumeauthority.Page{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return resumeauthority.Page{}, err
	}
	present, err := repository.ordinaryStatePresent(ctx)
	if err != nil {
		return resumeauthority.Page{}, err
	}
	if !present {
		return resumeauthority.NewPage(nil, resumeauthority.PageCursor{}, false)
	}
	runtime, mode, err := repository.openRuntime(ctx)
	if err != nil {
		return resumeauthority.Page{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, runtime.Close()) }()
	if !mode.Resumable() || runtime.registry == nil {
		return resumeauthority.NewPage(nil, resumeauthority.PageCursor{}, false)
	}

	pageCursor := checkpointstore.NewOperationPageCursor(cursor.After())
	page, err := runtime.registry.PageOperations(pageCursor, maximum)
	if err != nil {
		return resumeauthority.Page{}, nativeResumeError(err)
	}
	headers := make([]resumeauthority.Header, 0, len(page.Records()))
	for _, record := range page.Records() {
		header, headerErr := resumeauthority.NewHeader(record)
		if headerErr != nil {
			return resumeauthority.Page{}, headerErr
		}
		headers = append(headers, header)
	}
	var next resumeauthority.PageCursor
	if after, ok := page.Next().After(); ok {
		next = resumeauthority.NewPageCursor(after)
	}
	return resumeauthority.NewPage(headers, next, page.Unknown())
}

func (repository *NativeResumeRepository) Acquire(
	ctx context.Context,
	operation receivecontract.OperationID,
) (result resumeauthority.OperationLease, resultErr error) {
	if repository == nil || ctx == nil || repository.platformFactory == nil || operation.IsZero() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	present, err := repository.ordinaryStatePresent(ctx)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, fs.ErrNotExist
	}
	runtime, mode, err := repository.openRuntime(ctx)
	if err != nil {
		return nil, err
	}
	resources := &NativeResumeLease{runtime: runtime}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, resources.Close())
		}
	}()
	if !mode.Resumable() || runtime.registry == nil {
		return nil, fs.ErrNotExist
	}
	lease, err := runtime.registry.AcquireOperationLease(operation)
	if err != nil {
		return nil, nativeResumeError(err)
	}
	resources.operation = lease
	header, err := resumeauthority.NewHeader(lease.Record())
	if err != nil {
		return nil, err
	}
	resources.header = header
	return resources, nil
}

func (repository *NativeResumeRepository) ordinaryStatePresent(
	ctx context.Context,
) (present bool, resultErr error) {
	platform, err := repository.platformFactory(repository.rootPath, false)
	if err != nil {
		return false, err
	}
	if platform == nil || platform.Root() == nil {
		return false, errors.Join(outputcap.ErrRecoverableOutputUnsupported, closeNativeResumePlatform(platform))
	}
	defer func() { resultErr = errors.Join(resultErr, platform.Close()) }()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	root := platform.Root()
	kind, exact, err := root.ClassifyExactEntry(checkpointstore.ControlDirectory)
	if err != nil || !exact {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	if kind == outputcap.EntryAbsent {
		return false, nil
	}
	if kind != outputcap.EntryDirectory {
		return false, outputcap.ErrUnsafeNamespace
	}
	control, err := root.OpenDirectory(checkpointstore.ControlDirectory, true)
	if err != nil || control == nil {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace, closeNativeResumeDirectory(control))
	}
	defer func() { resultErr = errors.Join(resultErr, control.Close()) }()
	kind, exact, err = control.ClassifyExactEntry(checkpointstore.OrdinaryRegistryDirectoryV1)
	if err != nil || !exact {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	switch kind {
	case outputcap.EntryAbsent:
		return false, nil
	case outputcap.EntryDirectory:
		return true, nil
	default:
		return false, outputcap.ErrUnsafeNamespace
	}
}

func (repository *NativeResumeRepository) openRuntime(
	ctx context.Context,
) (*Authority, ExecutionMode, error) {
	runtime, err := New(Config{
		RootPath: repository.rootPath, CreateRoot: false,
		PlatformFactory: repository.platformFactory,
	})
	if err != nil {
		return nil, ExecutionMode{}, err
	}
	mode, err := runtime.BindDestination(ctx)
	if err != nil {
		return nil, ExecutionMode{}, errors.Join(err, runtime.Close())
	}
	return runtime, mode, nil
}

type NativeResumeLease struct {
	mu     sync.Mutex
	closed bool

	runtime    *Authority
	operation  *checkpointstore.OperationRegistryLease
	header     resumeauthority.Header
	topLevel   *destinationauthority.TopLevelReservation
	repository *checkpointstore.Repository
	store      *checkpointstore.FileExecutionStore
}

func (lease *NativeResumeLease) Snapshot(
	ctx context.Context,
) (resumeauthority.Snapshot, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if err := lease.validateLocked(ctx); err != nil {
		return resumeauthority.Snapshot{}, err
	}
	if err := lease.refreshHeaderLocked(); err != nil {
		return resumeauthority.Snapshot{}, err
	}
	record := lease.header.Record()
	switch record.Lifecycle() {
	case checkpointmodel.OrdinaryOperationCompleted,
		checkpointmodel.OrdinaryOperationDiscarded,
		checkpointmodel.OrdinaryOperationCleanupPending:
		return resumeauthority.NewSnapshot(lease.header, nil)
	}
	if err := lease.ensureTopLevelLocked(); err != nil {
		if record.Lifecycle() == checkpointmodel.OrdinaryOperationActive {
			if _, transitionErr := lease.transitionLocked(
				checkpointmodel.OrdinaryLifecycleRequireAttention,
				checkpointmodel.OrdinaryReasonDestinationOwnershipUnknown,
			); transitionErr != nil {
				return resumeauthority.Snapshot{}, errors.Join(err, transitionErr)
			}
			return resumeauthority.NewSnapshot(lease.header, nil)
		}
		return resumeauthority.NewSnapshot(lease.header, nil)
	}
	present, err := lease.ensureFileStoreLocked()
	if errors.Is(err, fs.ErrNotExist) || !present && err == nil {
		return resumeauthority.NewSnapshot(lease.header, nil)
	}
	if err != nil {
		if record.Lifecycle() == checkpointmodel.OrdinaryOperationActive &&
			nativeResumeUncertain(err) {
			if _, transitionErr := lease.transitionLocked(
				checkpointmodel.OrdinaryLifecycleRequireAttention,
				checkpointmodel.OrdinaryReasonOperationOwnershipUnknown,
			); transitionErr != nil {
				return resumeauthority.Snapshot{}, errors.Join(err, transitionErr)
			}
			return resumeauthority.NewSnapshot(lease.header, nil)
		}
		return resumeauthority.Snapshot{}, nativeResumeError(err)
	}
	items, err := ordinaryResumeItems(ctx, lease.topLevel, lease.store)
	if err != nil {
		return resumeauthority.Snapshot{}, nativeResumeError(err)
	}
	return resumeauthority.NewSnapshot(lease.header, items)
}

func (lease *NativeResumeLease) Transition(
	ctx context.Context,
	event checkpointmodel.OrdinaryLifecycleEvent,
	reason checkpointmodel.OrdinaryClosedReason,
) (resumeauthority.Header, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if err := lease.validateLocked(ctx); err != nil {
		return resumeauthority.Header{}, err
	}
	return lease.transitionLocked(event, reason)
}

func (lease *NativeResumeLease) transitionLocked(
	event checkpointmodel.OrdinaryLifecycleEvent,
	reason checkpointmodel.OrdinaryClosedReason,
) (resumeauthority.Header, error) {
	previous := lease.operation.Record()
	lifecycle, closedReason, err := checkpointmodel.ReduceOrdinaryOperationLifecycle(
		previous.Lifecycle(), event, reason,
	)
	if err != nil {
		return resumeauthority.Header{}, err
	}
	next, err := checkpointmodel.NextOrdinaryOperationRecord(
		previous,
		checkpointmodel.NextOrdinaryOperationRecordSpec{
			Lifecycle: lifecycle, Lease: checkpointmodel.OrdinaryLeaseHeld,
			ClosedReason: closedReason,
		},
	)
	if err != nil {
		return resumeauthority.Header{}, err
	}
	if err := lease.operation.Replace(previous, next); err != nil {
		return resumeauthority.Header{}, nativeResumeError(err)
	}
	header, err := resumeauthority.NewHeader(next)
	if err == nil {
		lease.header = header
	}
	return header, err
}

func (lease *NativeResumeLease) Cleanup(
	ctx context.Context,
) (resumeauthority.CleanupState, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if err := lease.validateLocked(ctx); err != nil {
		return 0, err
	}
	record := lease.operation.Record()
	if record.Lifecycle() != checkpointmodel.OrdinaryOperationCompleted &&
		record.Lifecycle() != checkpointmodel.OrdinaryOperationDiscarded &&
		record.Lifecycle() != checkpointmodel.OrdinaryOperationCleanupPending {
		return 0, transfer.ErrInvalidOutputBinding
	}
	present, err := lease.ensureFileStoreLocked()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nativeResumeCleanupFailure(err)
	}
	if present {
		if err := lease.store.CleanupOwned(ctx); err != nil {
			return nativeResumeCleanupFailure(err)
		}
	}
	if lease.repository != nil {
		if err := lease.repository.Close(); err != nil {
			return resumeauthority.CleanupPending, nativeResumeError(err)
		}
		lease.repository = nil
		lease.store = nil
	}
	if err := lease.operation.DeleteTerminal(); err != nil {
		if lease.operation.Deleted() {
			return resumeauthority.CleanupComplete, nativeResumeError(err)
		}
		return nativeResumeCleanupFailure(err)
	}
	return resumeauthority.CleanupComplete, nil
}

func (lease *NativeResumeLease) ensureFileStoreLocked() (bool, error) {
	if lease.store != nil && lease.repository != nil {
		return true, nil
	}
	record := lease.operation.Record()
	intent, err := record.VerifyIntent(transfer.DecodeReceiveIntent)
	if err != nil {
		return false, err
	}
	disposition, err := ordinaryResumeRootDisposition(intent)
	if err != nil {
		return false, err
	}
	ownership, err := lease.runtime.destination.FileCheckpointOwnership(disposition)
	if err != nil {
		return false, err
	}
	binding, err := checkpointmodel.NewBinding(
		ownership, record.OperationID(), record.ReceiveIntentDigest(), intent.BindingDigest(),
	)
	if err != nil {
		return false, err
	}
	repository, err := checkpointstore.OpenOrdinaryFileRepository(lease.operation, binding, false)
	if err != nil {
		return false, err
	}
	store, err := checkpointstore.NewFileExecutionStoreWithProfile(
		&repository, lease.runtime.destination.LiveCleanupProfile(),
	)
	if err != nil {
		return false, errors.Join(err, repository.Close())
	}
	lease.repository = &repository
	lease.store = store
	return true, nil
}

func (lease *NativeResumeLease) ensureTopLevelLocked() error {
	if lease.topLevel != nil {
		return nil
	}
	if lease.runtime == nil || lease.runtime.registry == nil ||
		lease.runtime.destination == nil {
		return transfer.ErrInvalidOutputBinding
	}
	record := lease.operation.Record()
	intent, err := record.VerifyIntent(transfer.DecodeReceiveIntent)
	if err != nil {
		return err
	}
	reservation, direct := intent.MaterializationPlan().DestinationReservation()
	proof, proofErr := lease.runtime.registry.RecoveryProof(record)
	if !direct || proofErr != nil || !proof.Valid() ||
		!validNamedReservation(reservation, intent.ArtifactSpec(), lease.runtime.binding) ||
		record.ReservationClaim().Token() != [32]byte(proof.Claim().Token) ||
		record.ReservationClaim().Generation() != proof.Claim().Generation {
		return errors.Join(proofErr, ErrNativeResumeOwnershipUnknown)
	}
	topLevel, err := lease.runtime.destination.ReopenTopLevel(
		destinationauthority.ExpectedReservation{
			Reservation: reservation, PersistentIdentityClaim: proof.PersistentIdentity(),
			MetadataClaim: proof.Claim(),
		},
	)
	if err != nil {
		return err
	}
	lease.topLevel = topLevel
	return nil
}

func ordinaryResumeRootDisposition(
	intent transfer.ReceiveIntent,
) (outputcap.RootOpenDisposition, error) {
	reservation, ok := intent.MaterializationPlan().DestinationReservation()
	if !ok {
		return "", transfer.ErrInvalidOutputBinding
	}
	switch reservation.EntryKind() {
	case receivecontract.ContainerEntrySingleFile:
		return outputcap.CallerProvidedContainer, nil
	case receivecontract.ContainerEntryResultRoot:
		return outputcap.AuthorityCreatedRoot, nil
	default:
		return "", transfer.ErrInvalidOutputBinding
	}
}

func (lease *NativeResumeLease) refreshHeaderLocked() error {
	header, err := resumeauthority.NewHeader(lease.operation.Record())
	if err == nil {
		lease.header = header
	}
	return err
}

func (lease *NativeResumeLease) validateLocked(ctx context.Context) error {
	if lease == nil || lease.closed || lease.runtime == nil || lease.runtime.destination == nil ||
		lease.runtime.registry == nil || lease.operation == nil || ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	return ctx.Err()
}

func (lease *NativeResumeLease) Close() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return nil
	}
	lease.closed = true
	err := errors.Join(
		closeNativeResumeRepository(lease.repository),
		closeNativeResumeTopLevel(lease.topLevel),
		closeNativeResumeOperationRegistryLease(lease.operation),
		closeNativeResumeRuntime(lease.runtime),
	)
	lease.repository = nil
	lease.store = nil
	lease.topLevel = nil
	lease.operation = nil
	lease.runtime = nil
	return nativeResumeError(err)
}

func closeNativeResumeRepository(repository *checkpointstore.Repository) error {
	if repository == nil {
		return nil
	}
	return repository.Close()
}

func closeNativeResumeTopLevel(reservation *destinationauthority.TopLevelReservation) error {
	if reservation == nil {
		return nil
	}
	return reservation.Close()
}

func closeNativeResumeOperationRegistryLease(lease *checkpointstore.OperationRegistryLease) error {
	if lease == nil {
		return nil
	}
	return lease.Close()
}

func closeNativeResumePlatform(platform outputcap.Platform) error {
	if platform == nil {
		return nil
	}
	return platform.Close()
}

func closeNativeResumeRuntime(runtime *Authority) error {
	if runtime == nil {
		return nil
	}
	return runtime.Close()
}

var _ resumeauthority.Store = (*NativeResumeRepository)(nil)
var _ resumeauthority.OperationLease = (*NativeResumeLease)(nil)
