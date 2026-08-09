package outputruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type dispositionPlatform struct {
	outputcap.Platform
	disposition outputcap.RootOpenDisposition
}

func (platform dispositionPlatform) RootOpenDisposition() outputcap.RootOpenDisposition {
	return platform.disposition
}

// nativeOutputResources owns the entire live capability graph. The operation
// lease closes last among repository capabilities, so no second process can
// observe a half-released session and mistake it for resumable authority.
type nativeOutputResources struct {
	once sync.Once
	err  error

	authority     *Authority
	intent        transfer.ReceiveIntentDigest
	operation     receivecontract.OperationID
	sessionID     transfer.OutputSessionID
	certification FilesystemOutputCertificationID
	directories   *directoryauthority.Authority
	repository    *checkpointstore.Repository
	lease         *checkpointstore.OperationLease
	namespace     *checkpointstore.Namespace
	platform      outputcap.Platform
	leaseHeld     bool
}

func (resources *nativeOutputResources) ReleaseOutputSession(ctx context.Context) error {
	if resources == nil {
		return nil
	}
	resources.once.Do(func() {
		var releaseErr error
		if resources.directories != nil {
			releaseErr = errors.Join(releaseErr, resources.directories.Close())
		}
		if resources.repository != nil {
			releaseErr = errors.Join(releaseErr, resources.repository.Close())
		}
		if resources.lease != nil {
			leaseErr := resources.lease.Close()
			releaseErr = errors.Join(releaseErr, leaseErr)
			if resources.leaseHeld && resources.authority != nil {
				milestone := FilesystemOutputNativeLockReleased
				if leaseErr != nil {
					milestone = FilesystemOutputNativeLockReleaseReportedFailure
				}
				resources.authority.trace(FilesystemOutputTrace{
					Operation: TraceNativeLock, ReceiveIntentDigest: resources.intent,
					ReceiveOperationID: resources.operation, SessionID: resources.sessionID,
					Certification:   resources.certification,
					NativeLockScope: FilesystemOutputNativeLockSession, NativeLockMilestone: milestone,
					RuntimeComponent: FilesystemOutputRuntimeCheckpoint,
					RuntimeOperation: FilesystemOutputRuntimeAcquireOperationLease,
					RuntimeDecision:  FilesystemOutputRuntimeClosed, Failed: leaseErr != nil,
				})
			}
		}
		if resources.namespace != nil {
			releaseErr = errors.Join(releaseErr, resources.namespace.Close())
		}
		if resources.platform != nil {
			releaseErr = errors.Join(releaseErr, resources.platform.Close())
		}
		if releaseErr != nil {
			resources.err = runtimeOutputError(ctx, transferfault.OutputStateIO, "release native output resources", releaseErr)
		}
	})
	return resources.err
}

type certifiedNativePlatform struct {
	platform      outputcap.Platform
	certification FilesystemOutputCertificationID
	rootBinding   outputcap.OutputRootBinding
	authorityRef  receivecontract.AuthorityRef
}

type nativeCheckpointState struct {
	ownership           checkpointmodel.Ownership
	rootOpenDisposition outputcap.RootOpenDisposition
	fileStore           *checkpointstore.FileExecutionStore
	sessionID           transfer.OutputSessionID
	lifecycle           *nativeLifecycleRecorder
}

func (authority *Authority) openNativeOutput(
	ctx context.Context,
	intent transfer.ReceiveIntent,
) (transfer.DirectTreeSession, error) {
	if err := validateNativeOutputRequest(authority, ctx, intent); err != nil {
		return nil, err
	}
	resources := &nativeOutputResources{
		authority: authority, intent: intent.Digest(), operation: intent.OperationID(),
	}
	committed := false
	defer func() {
		if !committed {
			_ = resources.ReleaseOutputSession(context.Background())
		}
	}()

	platform, err := authority.acquireCertifiedNativePlatform(ctx, intent.Digest(), resources)
	if err != nil {
		return nil, err
	}
	if err := validateIntentForCertifiedRoot(intent, platform.authorityRef); err != nil {
		return nil, err
	}
	checkpoints, err := authority.acquireNativeCheckpointState(ctx, intent, platform, resources)
	if err != nil {
		return nil, err
	}
	session, err := authority.assembleNativeOutputSession(ctx, intent, platform, checkpoints, resources)
	if err != nil {
		return nil, err
	}
	authority.publishNativeOutputOpened(intent, platform, checkpoints)
	committed = true
	return session, nil
}

func validateNativeOutputRequest(authority *Authority, ctx context.Context, intent transfer.ReceiveIntent) error {
	if authority == nil || ctx == nil || intent.IsZero() || authority.platformFactory == nil ||
		authority.rootPath == "" || !filepath.IsAbs(authority.rootPath) ||
		filepath.Clean(authority.rootPath) != authority.rootPath || authority.sessionIDs == nil ||
		authority.random == nil || authority.now == nil {
		return transfer.ErrInvalidReceiveIntent
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return validateDirectTreeCatalogRoot(intent)
}

func validateDirectTreeCatalogRoot(intent transfer.ReceiveIntent) error {
	if intent.IsZero() || intent.MaterializationPlan().Kind() != receivecontract.PlanDirectTree {
		return transfer.ErrInvalidOutputBinding
	}
	layout, tree := intent.ArtifactSpec().DirectoryTree()
	reservation, reserved := intent.MaterializationPlan().DestinationReservation()
	if !tree || layout.Kind() != receivecontract.DirectoryTreeCatalogRoot || !reserved ||
		reservation.Kind() != receivecontract.ReservationContainerRoot ||
		reservation.AuthorityKind() != receivecontract.AuthorityNativeContainer ||
		reservation.Guarantees().Profile() != receivecontract.GuaranteeNativeTree ||
		reservation.OperationID() != intent.OperationID() || reservation.Digest() != intent.BindingDigest() {
		return transfer.ErrInvalidOutputBinding
	}
	return nil
}

func validateIntentForCertifiedRoot(intent transfer.ReceiveIntent, authority receivecontract.AuthorityRef) error {
	if err := validateDirectTreeCatalogRoot(intent); err != nil {
		return err
	}
	reservation, _ := intent.MaterializationPlan().DestinationReservation()
	if authority.IsZero() || reservation.AuthorityRef() != authority {
		return transfer.ErrInvalidOutputBinding
	}
	return nil
}

func (authority *Authority) acquireCertifiedNativePlatform(
	ctx context.Context,
	intentDigest transfer.ReceiveIntentDigest,
	resources *nativeOutputResources,
) (certifiedNativePlatform, error) {
	platform, err := authority.platformFactory(authority.rootPath, authority.createRoot)
	if err != nil {
		return certifiedNativePlatform{}, runtimeOutputError(ctx, transferfault.OutputOwnership, "certify output filesystem", err)
	}
	resources.platform = platform
	if platform == nil || platform.Root() == nil {
		return certifiedNativePlatform{}, runtimeOutputError(ctx, transferfault.OutputOwnership, "certify output filesystem", transfer.ErrInvalidOutputBinding)
	}
	certification := filesystemOutputCertificationFromState(platform.Certification())
	resources.certification = certification
	if certification == "" {
		return certifiedNativePlatform{}, runtimeOutputError(ctx, transferfault.OutputUnsupportedFilesystem, "certify output filesystem", outputcap.ErrRecoverableOutputUnsupported)
	}
	authority.trace(FilesystemOutputTrace{
		Operation: TraceFilesystemCertified, ReceiveIntentDigest: intentDigest,
		Certification: certification, RootOpenDisposition: runtimeRootDisposition(platform.RootOpenDisposition()),
		RuntimeComponent: FilesystemOutputRuntimeSession,
		RuntimeOperation: FilesystemOutputRuntimeOpenDirectTree,
		RuntimeDecision:  FilesystemOutputRuntimeValidated,
	})
	if err := validateOutputCreateAuthority(platform.Root()); err != nil {
		return certifiedNativePlatform{}, runtimeOutputError(ctx, transferfault.OutputOwnership, "validate output create authority", err)
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		return certifiedNativePlatform{}, runtimeOutputError(ctx, transferfault.OutputUnsupportedFilesystem, "probe output filesystem", err)
	}
	if platform.Durability() == transfer.DurabilityNone {
		return certifiedNativePlatform{}, runtimeOutputError(ctx, transferfault.OutputUnsupportedFilesystem, "probe output durability", outputcap.ErrRecoverableOutputUnsupported)
	}
	rootBinding, err := platform.RootBinding()
	if err != nil || rootBinding.IsZero() {
		return certifiedNativePlatform{}, runtimeOutputError(ctx, transferfault.OutputOwnership, "bind output root", errors.Join(err, transfer.ErrInvalidOutputBinding))
	}
	authorityRef, err := receivecontract.AuthorityRefFromBytes(rootBinding.Bytes())
	if err != nil {
		return certifiedNativePlatform{}, runtimeOutputError(ctx, transferfault.OutputOwnership, "bind output root authority", err)
	}
	authority.trace(FilesystemOutputTrace{
		Operation: TraceFeatureProbeCompleted, ReceiveIntentDigest: intentDigest,
		Certification: certification, RootOpenDisposition: runtimeRootDisposition(platform.RootOpenDisposition()),
		RuntimeComponent: FilesystemOutputRuntimeSession,
		RuntimeOperation: FilesystemOutputRuntimeOpenDirectTree,
		RuntimeDecision:  FilesystemOutputRuntimeValidated,
	})
	return certifiedNativePlatform{
		platform: platform, certification: certification, rootBinding: rootBinding, authorityRef: authorityRef,
	}, nil
}

func (authority *Authority) acquireNativeCheckpointState(
	ctx context.Context,
	intent transfer.ReceiveIntent,
	platform certifiedNativePlatform,
	resources *nativeOutputResources,
) (nativeCheckpointState, error) {
	namespace, ownership, err := openNativeCheckpointNamespace(platform.platform, platform.authorityRef)
	if err != nil {
		return nativeCheckpointState{}, checkpointRuntimeError(ctx, "open checkpoint namespace", err)
	}
	resources.namespace = &namespace
	lease, err := namespace.AcquireOperation(intent.OperationID(), intent.Digest(), intent.BindingDigest())
	if err != nil {
		authority.traceNativeLeaseFailure(intent, platform, err)
		return nativeCheckpointState{}, checkpointRuntimeError(ctx, "acquire checkpoint operation lease", err)
	}
	resources.lease = &lease
	resources.leaseHeld = true
	authority.trace(FilesystemOutputTrace{
		Operation: TraceNativeLock, ReceiveIntentDigest: intent.Digest(), ReceiveOperationID: intent.OperationID(),
		Certification: platform.certification, NativeLockScope: FilesystemOutputNativeLockSession,
		NativeLockMilestone: FilesystemOutputNativeLockAcquired,
		RuntimeComponent:    FilesystemOutputRuntimeCheckpoint,
		RuntimeOperation:    FilesystemOutputRuntimeAcquireOperationLease,
		RuntimeDecision:     FilesystemOutputRuntimeReserved,
	})
	repository, err := lease.OpenExistingRepository()
	if err != nil {
		return nativeCheckpointState{}, checkpointRuntimeError(ctx, "open checkpoint operation repository", err)
	}
	resources.repository = &repository
	if err := verifyStoredOperation(&repository, intent); err != nil {
		return nativeCheckpointState{}, checkpointRuntimeError(ctx, "verify checkpoint operation", err)
	}
	lifecycle, err := newNativeLifecycleRecorder(authority, intent, ownership, &repository)
	if err != nil {
		return nativeCheckpointState{}, checkpointRuntimeError(ctx, "open receive lifecycle", err)
	}
	if err := lifecycle.Activate(ctx); err != nil {
		return nativeCheckpointState{}, checkpointRuntimeError(ctx, "activate receive lifecycle", err)
	}
	fileStore, err := checkpointstore.NewFileExecutionStore(&repository)
	if err != nil {
		return nativeCheckpointState{}, checkpointRuntimeError(ctx, "reconcile checkpoint operation repository", err)
	}
	sessionID, err := authority.sessionIDs.NewOutputSessionID()
	if err != nil || sessionID.IsZero() {
		return nativeCheckpointState{}, runtimeOutputError(ctx, transferfault.OutputStateIO, "generate output session identity", errors.Join(err, transfer.ErrInvalidOutputBinding))
	}
	resources.sessionID = sessionID
	authority.trace(FilesystemOutputTrace{
		Operation: TraceCheckpointReconciled, ReceiveIntentDigest: intent.Digest(),
		ReceiveOperationID: intent.OperationID(), SessionID: sessionID,
		Certification: platform.certification, RootOpenDisposition: runtimeRootDisposition(ownership.RootOpenDisposition()),
		RuntimeComponent:      FilesystemOutputRuntimeCheckpoint,
		RuntimeOperation:      FilesystemOutputRuntimeReconcileCheckpoints,
		RuntimeDecision:       FilesystemOutputRuntimeReconciled,
		CheckpointRecordCount: fileStore.RecordCount(),
	})
	return nativeCheckpointState{
		ownership: ownership, rootOpenDisposition: ownership.RootOpenDisposition(), fileStore: fileStore,
		sessionID: sessionID, lifecycle: lifecycle,
	}, nil
}

func verifyStoredOperation(repository *checkpointstore.Repository, intent transfer.ReceiveIntent) error {
	record, err := repository.ReadOperation()
	if err != nil {
		return err
	}
	stored, err := record.VerifyIntent(transfer.DecodeReceiveIntent)
	if err != nil || !stored.EqualCanonical(intent) {
		return errors.Join(checkpointmodel.ErrRecordBinding, err)
	}
	reservation, _ := intent.MaterializationPlan().DestinationReservation()
	encoded, err := repository.ReadMaterializationBinding()
	if err != nil || !bytes.Equal(encoded, reservation.CanonicalBytes()) {
		return errors.Join(checkpointmodel.ErrRecordBinding, err)
	}
	return nil
}

func (authority *Authority) traceNativeLeaseFailure(intent transfer.ReceiveIntent, platform certifiedNativePlatform, err error) {
	milestone := FilesystemOutputNativeLockAcquireFailed
	var checkpointErr *checkpointstore.Error
	if errors.As(err, &checkpointErr) && checkpointErr.Code() == checkpointstore.ErrorBusy {
		milestone = FilesystemOutputNativeLockContended
	}
	event := FilesystemOutputTrace{
		Operation: TraceNativeLock, ReceiveIntentDigest: intent.Digest(), ReceiveOperationID: intent.OperationID(),
		Certification: platform.certification, NativeLockScope: FilesystemOutputNativeLockSession,
		NativeLockMilestone: milestone, RuntimeComponent: FilesystemOutputRuntimeCheckpoint,
		RuntimeOperation: FilesystemOutputRuntimeAcquireOperationLease,
		RuntimeDecision:  FilesystemOutputRuntimeRejected, Failed: true,
	}
	applyRuntimeFault(&event, checkpointRuntimeFault(err))
	authority.trace(event)
}

type persistedDirectoryExecutor struct {
	directories *directoryauthority.Authority
	repository  *checkpointstore.Repository
	intent      transfer.ReceiveIntent
}

func (executor persistedDirectoryExecutor) MaterializeDirectory(
	ctx context.Context,
	claim outputsession.DirectoryClaim,
) (outputsession.DirectoryMaterialization, error) {
	observation, err := executor.directories.MaterializeDirectory(ctx, claim)
	if err != nil || observation.Cut != outputsession.MutationStable {
		return observation, err
	}
	owned, err := executor.directories.OwnedDirectoryID(claim)
	if err != nil {
		return outputsession.DirectoryMaterialization{Cut: outputsession.MutationAmbiguous}, err
	}
	record, err := checkpointmodel.NewAdmittedDirectory(executor.intent, claim.Admission(), owned)
	if err != nil {
		return outputsession.DirectoryMaterialization{Cut: outputsession.MutationAmbiguous}, err
	}
	if err := installAdmittedDirectory(executor.repository, record); err != nil {
		return outputsession.DirectoryMaterialization{Cut: outputsession.MutationAmbiguous}, err
	}
	return observation, nil
}

func installAdmittedDirectory(repository *checkpointstore.Repository, proposed checkpointmodel.AdmittedDirectory) error {
	existing, err := repository.ReadAdmittedDirectory(proposed.DirectoryID())
	if err == nil {
		if sameAdmittedDirectoryObject(existing, proposed) {
			return nil
		}
		return checkpointmodel.ErrRecordBinding
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	installErr := repository.InstallAdmittedDirectory(proposed)
	if installErr == nil {
		return nil
	}
	// A racing admission is safe only when both executions independently proved
	// the same catalog generation and persistent native object. Runtime HMAC
	// admissions are intentionally session-local and therefore are not compared.
	existing, readErr := repository.ReadAdmittedDirectory(proposed.DirectoryID())
	if readErr == nil && sameAdmittedDirectoryObject(existing, proposed) {
		return nil
	}
	return errors.Join(installErr, readErr)
}

func sameAdmittedDirectoryObject(left, right checkpointmodel.AdmittedDirectory) bool {
	return left.Valid() && right.Valid() && left.OperationID() == right.OperationID() &&
		left.ReceiveIntentDigest() == right.ReceiveIntentDigest() && left.LayoutVersion() == right.LayoutVersion() &&
		left.Layout() == right.Layout() && left.DirectoryID() == right.DirectoryID() &&
		left.Generation() == right.Generation() && left.CanonicalPath() == right.CanonicalPath() &&
		left.ModifiedTime() == right.ModifiedTime() && left.OwnedObjectID() == right.OwnedObjectID()
}

func (executor persistedDirectoryExecutor) FinalizeDirectory(
	ctx context.Context,
	claim outputsession.DirectoryClaim,
) (outputsession.DirectoryFinalization, error) {
	return executor.directories.FinalizeDirectory(ctx, claim)
}

func (authority *Authority) assembleNativeOutputSession(
	ctx context.Context,
	intent transfer.ReceiveIntent,
	platform certifiedNativePlatform,
	checkpoints nativeCheckpointState,
	resources *nativeOutputResources,
) (*outputsession.Session, error) {
	runtimePlatform := dispositionPlatform{Platform: platform.platform, disposition: checkpoints.rootOpenDisposition}
	directories, err := directoryauthority.New(runtimePlatform, directoryauthority.Config{
		Trace: authority.directoryRuntimeTrace(intent.Digest(), checkpoints.sessionID),
	})
	if err != nil {
		return nil, runtimeDependencyError("construct directory authority", err)
	}
	resources.directories = directories
	fileAuthority, err := directoryauthority.NewFileAuthority(directories, checkpoints.fileStore)
	if err != nil {
		return nil, runtimeDependencyError("construct file destination authority", err)
	}
	files, err := fileexecution.New(fileexecution.Config{
		Intent: intent, Ownership: checkpoints.ownership, SessionID: checkpoints.sessionID,
		Directories: fileAuthority, Platform: checkpoints.fileStore, Checkpoints: checkpoints.fileStore,
		Random: authority.random, Trace: authority.fileRuntimeTrace(),
	})
	if err != nil {
		return nil, runtimeDependencyError("construct file execution engine", err)
	}
	secret, err := newNativeReceiptSecret(authority.random)
	if err != nil {
		return nil, runtimeOutputError(ctx, transferfault.OutputStateIO, "generate directory admission secret", err)
	}
	capabilities, err := transfer.NewDirectTreeCapabilities(transfer.DirectTreeCapabilities{
		Durability: platform.platform.Durability(), RandomWrite: true,
		FileFailureIsolation: true, ModifiedTime: true,
	})
	if err != nil {
		return nil, runtimeDependencyError("construct DirectTree capabilities", err)
	}
	directoryExecutor := persistedDirectoryExecutor{
		directories: directories, repository: resources.repository, intent: intent,
	}
	session, err := outputsession.New(outputsession.Config{
		Intent: intent, SessionID: checkpoints.sessionID, Capabilities: capabilities, ReceiptSecret: secret[:],
		Locator: directories, Directories: directoryExecutor, Files: newFileExecutionAdapter(files),
		Resources: resources, Lifecycle: checkpoints.lifecycle, Trace: authority.outputSessionRuntimeTrace(),
	})
	if err != nil {
		return nil, runtimeDependencyError("construct DirectTree session", err)
	}
	return session, nil
}

func (authority *Authority) publishNativeOutputOpened(
	intent transfer.ReceiveIntent,
	platform certifiedNativePlatform,
	checkpoints nativeCheckpointState,
) {
	common := FilesystemOutputTrace{
		ReceiveIntentDigest: intent.Digest(), ReceiveOperationID: intent.OperationID(),
		SessionID: checkpoints.sessionID, Certification: platform.certification,
		RootOpenDisposition: runtimeRootDisposition(checkpoints.rootOpenDisposition),
	}
	checkpointEvent := common
	checkpointEvent.Operation = TraceCheckpointNamespaceOpened
	checkpointEvent.RuntimeComponent = FilesystemOutputRuntimeCheckpoint
	checkpointEvent.RuntimeOperation = FilesystemOutputRuntimeOpenDirectTree
	checkpointEvent.RuntimeDecision = FilesystemOutputRuntimeSucceeded
	checkpointEvent.CheckpointRecordCount = checkpoints.fileStore.RecordCount()
	authority.trace(checkpointEvent)
	sessionEvent := common
	sessionEvent.Operation = TraceSessionOpened
	sessionEvent.RuntimeComponent = FilesystemOutputRuntimeSession
	sessionEvent.RuntimeOperation = FilesystemOutputRuntimeOpenDirectTree
	sessionEvent.RuntimeDecision = FilesystemOutputRuntimeActive
	authority.trace(sessionEvent)
}

func newNativeReceiptSecret(random io.Reader) ([sha256.Size]byte, error) {
	var secret [sha256.Size]byte
	if random == nil {
		return secret, transfer.ErrInvalidOutputBinding
	}
	if _, err := io.ReadFull(random, secret[:]); err != nil {
		return [sha256.Size]byte{}, err
	}
	if secret == ([sha256.Size]byte{}) {
		return [sha256.Size]byte{}, transfer.ErrInvalidOutputBinding
	}
	return secret, nil
}

func runtimeRootDisposition(disposition outputcap.RootOpenDisposition) FilesystemOutputRootDisposition {
	switch disposition {
	case outputcap.CallerProvidedContainer:
		return FilesystemOutputCallerProvidedContainer
	case outputcap.AuthorityCreatedRoot:
		return FilesystemOutputAuthorityCreatedRoot
	default:
		return ""
	}
}

func runtimeOutputError(ctx context.Context, code transferfault.OutputCode, operation string, cause error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(cause, context.Canceled) {
		return context.Canceled
	}
	value, _ := transferfault.NewOutput(transferfault.ScopeOutputPause, code)
	return transferfault.Wrap(value, fmt.Errorf("%s: %w", operation, cause))
}

func runtimeDependencyError(operation string, cause error) error {
	value, _ := transferfault.NewSession(transferfault.ScopeOutputPause, transferfault.SessionDependencyContract)
	return transferfault.Wrap(value, fmt.Errorf("%s: %w", operation, cause))
}
