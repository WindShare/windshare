package outputruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
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
)

type dispositionPlatform struct {
	outputcap.Platform
	disposition outputcap.RootOpenDisposition
}

func (platform dispositionPlatform) RootOpenDisposition() outputcap.RootOpenDisposition {
	return platform.disposition
}

// nativeOutputResources is the sole owner of the native graph after OpenOutput
// succeeds. outputsession invokes it only after closing admission and draining
// active operations, which keeps the intent lease authoritative through every
// stable file pause and the terminal job settlement.
type nativeOutputResources struct {
	once sync.Once
	err  error

	authority     *Authority
	intent        transfer.TransferIntentDigest
	sessionID     transfer.OutputSessionID
	certification FilesystemOutputCertificationID
	directories   *directoryauthority.Authority
	repository    *checkpointstore.Repository
	lease         *checkpointstore.IntentLease
	namespace     *checkpointstore.Namespace
	platform      outputcap.Platform
	leaseHeld     bool
}

type certifiedNativePlatform struct {
	platform      outputcap.Platform
	certification FilesystemOutputCertificationID
	rootBinding   outputcap.OutputRootBinding
}

type nativeCheckpointState struct {
	ownership           checkpointmodel.Ownership
	rootOpenDisposition outputcap.RootOpenDisposition
	fileStore           *checkpointstore.FileExecutionStore
	sessionID           transfer.OutputSessionID
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
					Operation: TraceNativeLock, IntentDigest: resources.intent,
					SessionID: resources.sessionID, Certification: resources.certification,
					NativeLockScope: FilesystemOutputNativeLockSession, NativeLockMilestone: milestone,
					RuntimeComponent: FilesystemOutputRuntimeCheckpoint,
					RuntimeOperation: FilesystemOutputRuntimeAcquireIntentLease,
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
			resources.err = runtimeOutputError(
				ctx, transferfault.OutputStateIO, "release native output resources", releaseErr,
			)
		}
	})
	return resources.err
}

func (authority *Authority) openNativeOutput(
	ctx context.Context,
	intent transfer.TransferIntent,
) (transfer.OutputSession, error) {
	if err := validateNativeOutputRequest(authority, ctx, intent); err != nil {
		return nil, err
	}

	resources := &nativeOutputResources{authority: authority, intent: intent.Digest()}
	committed := false
	defer func() {
		if !committed {
			_ = resources.ReleaseOutputSession(context.Background())
		}
	}()

	platform, err := authority.acquireCertifiedNativePlatform(ctx, intent, resources)
	if err != nil {
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

func validateNativeOutputRequest(
	authority *Authority,
	ctx context.Context,
	intent transfer.TransferIntent,
) error {
	if authority == nil || ctx == nil || intent.IsZero() || authority.platformFactory == nil ||
		authority.rootPath == "" || authority.sessionIDs == nil || authority.random == nil {
		return transfer.ErrInvalidTransferIntent
	}
	target := intent.OutputTarget()
	if target.Kind() != transfer.OutputFilesystemRootTarget || target.RootPath() == "" ||
		!filepath.IsAbs(target.RootPath()) || filepath.Clean(target.RootPath()) != filepath.Clean(authority.rootPath) ||
		intent.BackendID() != filesystemOutputBackendID || intent.Format() != transfer.OutputNativeTree {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (authority *Authority) acquireCertifiedNativePlatform(
	ctx context.Context,
	intent transfer.TransferIntent,
	resources *nativeOutputResources,
) (certifiedNativePlatform, error) {
	platform, err := authority.platformFactory(authority.rootPath, authority.createRoot)
	if err != nil {
		return certifiedNativePlatform{}, runtimeOutputError(
			ctx, transferfault.OutputOwnership, "certify output filesystem", err,
		)
	}
	resources.platform = platform
	if platform == nil || platform.Root() == nil {
		return certifiedNativePlatform{}, runtimeOutputError(
			ctx, transferfault.OutputOwnership, "certify output filesystem", transfer.ErrInvalidOutputBinding,
		)
	}
	certification := filesystemOutputCertificationFromState(platform.Certification())
	resources.certification = certification
	authority.trace(FilesystemOutputTrace{
		Operation: TraceFilesystemCertified, IntentDigest: intent.Digest(), Certification: certification,
		RootOpenDisposition: runtimeRootDisposition(platform.RootOpenDisposition()),
		RuntimeComponent:    FilesystemOutputRuntimeSession,
		RuntimeOperation:    FilesystemOutputRuntimeOpenOutput,
		RuntimeDecision:     FilesystemOutputRuntimeValidated,
	})
	if err := validateOutputCreateAuthority(platform.Root()); err != nil {
		return certifiedNativePlatform{}, runtimeOutputError(
			ctx, transferfault.OutputOwnership, "validate output create authority", err,
		)
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		return certifiedNativePlatform{}, runtimeOutputError(
			ctx, transferfault.OutputUnsupportedFilesystem, "probe output filesystem", err,
		)
	}
	if platform.Durability() == transfer.DurabilityNone {
		return certifiedNativePlatform{}, runtimeOutputError(
			ctx, transferfault.OutputUnsupportedFilesystem, "probe output durability", outputcap.ErrRecoverableOutputUnsupported,
		)
	}
	authority.trace(FilesystemOutputTrace{
		Operation: TraceFeatureProbeCompleted, IntentDigest: intent.Digest(), Certification: certification,
		RootOpenDisposition: runtimeRootDisposition(platform.RootOpenDisposition()),
		RuntimeComponent:    FilesystemOutputRuntimeSession,
		RuntimeOperation:    FilesystemOutputRuntimeOpenOutput,
		RuntimeDecision:     FilesystemOutputRuntimeValidated,
	})

	rootBinding, err := platform.RootBinding()
	if err != nil || rootBinding.IsZero() {
		if err == nil {
			err = transfer.ErrInvalidOutputBinding
		}
		return certifiedNativePlatform{}, runtimeOutputError(
			ctx, transferfault.OutputOwnership, "bind output root", err,
		)
	}
	return certifiedNativePlatform{
		platform: platform, certification: certification, rootBinding: rootBinding,
	}, nil
}

func (authority *Authority) acquireNativeCheckpointState(
	ctx context.Context,
	intent transfer.TransferIntent,
	platform certifiedNativePlatform,
	resources *nativeOutputResources,
) (nativeCheckpointState, error) {
	namespace, ownership, err := initializeNativeCheckpointNamespace(
		platform.platform, platform.rootBinding.Bytes(),
	)
	if err != nil {
		return nativeCheckpointState{}, checkpointRuntimeError(ctx, "initialize checkpoint namespace", err)
	}
	resources.namespace = &namespace
	persistedDisposition := ownership.RootOpenDisposition()

	lease, err := namespace.AcquireIntent(intent.Digest())
	if err != nil {
		milestone := FilesystemOutputNativeLockAcquireFailed
		var checkpointErr *checkpointstore.Error
		if errors.As(err, &checkpointErr) && checkpointErr.Code() == checkpointstore.ErrorBusy {
			milestone = FilesystemOutputNativeLockContended
		}
		trace := FilesystemOutputTrace{
			Operation: TraceNativeLock, IntentDigest: intent.Digest(),
			Certification: platform.certification, NativeLockScope: FilesystemOutputNativeLockSession,
			NativeLockMilestone: milestone, RuntimeComponent: FilesystemOutputRuntimeCheckpoint,
			RuntimeOperation: FilesystemOutputRuntimeAcquireIntentLease,
			RuntimeDecision:  FilesystemOutputRuntimeRejected, Failed: true,
		}
		applyRuntimeFault(&trace, checkpointRuntimeFault(err))
		authority.trace(trace)
		return nativeCheckpointState{}, checkpointRuntimeError(ctx, "acquire checkpoint intent lease", err)
	}
	resources.lease = &lease
	resources.leaseHeld = true
	authority.trace(FilesystemOutputTrace{
		Operation: TraceNativeLock, IntentDigest: intent.Digest(),
		Certification: platform.certification, NativeLockScope: FilesystemOutputNativeLockSession,
		NativeLockMilestone: FilesystemOutputNativeLockAcquired,
		RuntimeComponent:    FilesystemOutputRuntimeCheckpoint,
		RuntimeOperation:    FilesystemOutputRuntimeAcquireIntentLease,
		RuntimeDecision:     FilesystemOutputRuntimeReserved,
	})

	repository, err := lease.OpenOrCreateRepository()
	if err != nil {
		return nativeCheckpointState{}, checkpointRuntimeError(ctx, "open checkpoint intent repository", err)
	}
	resources.repository = &repository
	fileStore, err := checkpointstore.NewFileExecutionStore(&repository)
	if err != nil {
		return nativeCheckpointState{}, checkpointRuntimeError(ctx, "reconcile checkpoint intent repository", err)
	}
	sessionID, err := authority.sessionIDs.NewOutputSessionID()
	if err != nil || sessionID.IsZero() {
		if err == nil {
			err = transfer.ErrInvalidOutputBinding
		}
		return nativeCheckpointState{}, runtimeOutputError(
			ctx, transferfault.OutputStateIO, "generate output session identity", err,
		)
	}
	resources.sessionID = sessionID
	authority.trace(FilesystemOutputTrace{
		Operation: TraceCheckpointReconciled, IntentDigest: intent.Digest(), SessionID: sessionID,
		Certification: platform.certification, RootOpenDisposition: runtimeRootDisposition(persistedDisposition),
		RuntimeComponent:      FilesystemOutputRuntimeCheckpoint,
		RuntimeOperation:      FilesystemOutputRuntimeReconcileCheckpoints,
		RuntimeDecision:       FilesystemOutputRuntimeReconciled,
		CheckpointRecordCount: fileStore.RecordCount(),
	})
	return nativeCheckpointState{
		ownership: ownership, rootOpenDisposition: persistedDisposition,
		fileStore: fileStore, sessionID: sessionID,
	}, nil
}

func (authority *Authority) assembleNativeOutputSession(
	ctx context.Context,
	intent transfer.TransferIntent,
	platform certifiedNativePlatform,
	checkpoints nativeCheckpointState,
	resources *nativeOutputResources,
) (*outputsession.Session, error) {
	runtimePlatform := dispositionPlatform{
		Platform: platform.platform, disposition: checkpoints.rootOpenDisposition,
	}
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
	capabilities, err := transfer.NewOutputCapabilities(transfer.OutputCapabilities{
		Durability: platform.platform.Durability(), Mode: transfer.OutputNativeTree,
		RandomWrite: true, FileFailureIsolation: true, ModifiedTime: true,
		ArchiveBoundary: transfer.ArchiveFailureNotApplicable,
	})
	if err != nil {
		return nil, runtimeDependencyError("construct output capabilities", err)
	}
	session, err := outputsession.New(outputsession.Config{
		Intent: intent, SessionID: checkpoints.sessionID, Capabilities: capabilities, ReceiptSecret: secret[:],
		Locator: directories, Directories: directories, Files: files, Resources: resources,
		Trace: authority.outputSessionRuntimeTrace(),
	})
	if err != nil {
		return nil, runtimeDependencyError("construct output session", err)
	}
	return session, nil
}

func (authority *Authority) publishNativeOutputOpened(
	intent transfer.TransferIntent,
	platform certifiedNativePlatform,
	checkpoints nativeCheckpointState,
) {
	authority.trace(FilesystemOutputTrace{
		Operation: TraceCheckpointNamespaceOpened, IntentDigest: intent.Digest(), SessionID: checkpoints.sessionID,
		Certification:         platform.certification,
		RootOpenDisposition:   runtimeRootDisposition(checkpoints.rootOpenDisposition),
		RuntimeComponent:      FilesystemOutputRuntimeCheckpoint,
		RuntimeOperation:      FilesystemOutputRuntimeOpenOutput,
		RuntimeDecision:       FilesystemOutputRuntimeSucceeded,
		CheckpointRecordCount: checkpoints.fileStore.RecordCount(),
	})
	authority.trace(FilesystemOutputTrace{
		Operation: TraceSessionOpened, IntentDigest: intent.Digest(), SessionID: checkpoints.sessionID,
		Certification:       platform.certification,
		RootOpenDisposition: runtimeRootDisposition(checkpoints.rootOpenDisposition),
		RuntimeComponent:    FilesystemOutputRuntimeSession,
		RuntimeOperation:    FilesystemOutputRuntimeOpenOutput,
		RuntimeDecision:     FilesystemOutputRuntimeActive,
	})
}

func initializeNativeCheckpointNamespace(
	platform outputcap.Platform,
	rootIdentity []byte,
) (checkpointstore.Namespace, checkpointmodel.Ownership, error) {
	disposition := platform.RootOpenDisposition()
	ownership, err := nativeCheckpointOwnership(platform, rootIdentity, disposition)
	if err != nil {
		return checkpointstore.Namespace{}, checkpointmodel.Ownership{}, err
	}
	namespace, err := checkpointstore.Initialize(checkpointstore.CertifiedConfig{
		Root: platform.Root(), Ownership: ownership,
	})
	if err == nil || disposition != outputcap.CallerProvidedContainer {
		return namespace, ownership, err
	}
	var checkpointErr *checkpointstore.Error
	if !errors.As(err, &checkpointErr) || checkpointErr.Code() != checkpointstore.ErrorOwnershipMismatch {
		return checkpointstore.Namespace{}, checkpointmodel.Ownership{}, err
	}
	// A restart cannot infer creation authority from current path existence. An
	// exact ownership marker is the only fact allowed to recover that disposition.
	ownership, ownershipErr := nativeCheckpointOwnership(platform, rootIdentity, outputcap.AuthorityCreatedRoot)
	if ownershipErr != nil {
		return checkpointstore.Namespace{}, checkpointmodel.Ownership{}, ownershipErr
	}
	namespace, err = checkpointstore.Initialize(checkpointstore.CertifiedConfig{
		Root: platform.Root(), Ownership: ownership,
	})
	return namespace, ownership, err
}

func nativeCheckpointOwnership(
	platform outputcap.Platform,
	rootIdentity []byte,
	disposition outputcap.RootOpenDisposition,
) (checkpointmodel.Ownership, error) {
	return checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Backend:       filesystemOutputBackendID,
		Certification: platform.Certification(),
		RootIdentity:  rootIdentity, RootOpenDisposition: disposition,
	})
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

func checkpointRuntimeError(ctx context.Context, operation string, cause error) error {
	if cause == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	value := checkpointRuntimeFault(cause)
	return transferfault.Wrap(value, fmt.Errorf("%s: %w", operation, cause))
}

func checkpointRuntimeFault(cause error) transferfault.Fault {
	var checkpointErr *checkpointstore.Error
	if !errors.As(cause, &checkpointErr) || checkpointErr == nil {
		return transferfault.DependencyContractFault()
	}
	var code transferfault.CheckpointCode
	switch checkpointErr.Code() {
	case checkpointstore.ErrorBusy:
		code = transferfault.CheckpointBusy
	case checkpointstore.ErrorCorruptRecord:
		code = transferfault.CheckpointCorruptRecord
	case checkpointstore.ErrorUnsafeInstall:
		code = transferfault.CheckpointUnsafeInstall
	case checkpointstore.ErrorOwnershipMismatch:
		code = transferfault.CheckpointOwnershipMismatch
	case checkpointstore.ErrorStateIO:
		code = transferfault.CheckpointStateIO
	default:
		return transferfault.DependencyContractFault()
	}
	value, _ := transferfault.NewCheckpoint(transferfault.ScopeOutputPause, code)
	return value
}

func runtimeOutputError(
	ctx context.Context,
	code transferfault.OutputCode,
	operation string,
	cause error,
) error {
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
	value, _ := transferfault.NewSession(
		transferfault.ScopeOutputPause, transferfault.SessionDependencyContract,
	)
	return transferfault.Wrap(value, fmt.Errorf("%s: %w", operation, cause))
}
