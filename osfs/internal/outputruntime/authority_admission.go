package outputruntime

import (
	"context"
	"crypto/rand"
	"io"
	"sync"
	"time"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type FilesystemOutputTraceOperation uint8

const (
	TraceFilesystemIdentityEvidence FilesystemOutputTraceOperation = iota + 1
	TraceFeatureProbeCompleted
	TraceCheckpointNamespaceOpened
	TraceNativeLock
	TraceSessionOpened
	TraceCheckpointReconciled
	TraceRuntimeDecision
)

type FilesystemOutputRootDisposition string

const (
	FilesystemOutputCallerProvidedContainer FilesystemOutputRootDisposition = "caller-provided-container"
	FilesystemOutputAuthorityCreatedRoot    FilesystemOutputRootDisposition = "authority-created-root"
)

type FilesystemOutputIdentitySchemeID string

const (
	FilesystemOutputIdentitySchemeLinuxHandleV2     FilesystemOutputIdentitySchemeID = "linux/ext4/handle/v2"
	FilesystemOutputIdentitySchemeWindowsObjectIDV1 FilesystemOutputIdentitySchemeID = "windows/ntfs/object-id/v1"
	FilesystemOutputIdentitySchemeDarwinObjectIDV1  FilesystemOutputIdentitySchemeID = "darwin/apfs/object-id/v1"
)

func filesystemOutputIdentitySchemeFromState(identityScheme outputcap.IdentitySchemeID) FilesystemOutputIdentitySchemeID {
	switch identityScheme {
	case outputcap.IdentitySchemeLinuxHandleV2:
		return FilesystemOutputIdentitySchemeLinuxHandleV2
	case outputcap.IdentitySchemeWindowsObjectIDV1:
		return FilesystemOutputIdentitySchemeWindowsObjectIDV1
	case outputcap.IdentitySchemeDarwinObjectIDV1:
		return FilesystemOutputIdentitySchemeDarwinObjectIDV1
	default:
		return ""
	}
}

type FilesystemOutputRuntimeComponent uint8

const (
	FilesystemOutputRuntimeSession FilesystemOutputRuntimeComponent = iota + 1
	FilesystemOutputRuntimeDirectory
	FilesystemOutputRuntimeFile
	FilesystemOutputRuntimeCheckpoint
)

type FilesystemOutputRuntimeOperation uint8

const (
	FilesystemOutputRuntimeOpenDirectTree FilesystemOutputRuntimeOperation = iota + 1
	FilesystemOutputRuntimeAcquireOperationLease
	FilesystemOutputRuntimeReconcileCheckpoints
	FilesystemOutputRuntimeAdmitDirectory
	FilesystemOutputRuntimeFinalizeDirectory
	FilesystemOutputRuntimeBeginFile
	FilesystemOutputRuntimeWriteRange
	FilesystemOutputRuntimeCheckpointFile
	FilesystemOutputRuntimeCommitFile
	FilesystemOutputRuntimePauseFile
	FilesystemOutputRuntimeRetireFile
	FilesystemOutputRuntimePauseTree
	FilesystemOutputRuntimeFinalizeTree
	FilesystemOutputRuntimeMaterializeDirectory
	FilesystemOutputRuntimeCreateOwnedFile
	FilesystemOutputRuntimeRecoverFile
	FilesystemOutputRuntimePublishFile
	FilesystemOutputRuntimeQuarantineFile
	FilesystemOutputRuntimeAdmitDestination
	FilesystemOutputRuntimeFirstWrite
	FilesystemOutputRuntimeCleanup
)

type FilesystemOutputRuntimeDecision uint8

const (
	FilesystemOutputRuntimeValidated FilesystemOutputRuntimeDecision = iota + 1
	FilesystemOutputRuntimeReserved
	FilesystemOutputRuntimeCoalesced
	FilesystemOutputRuntimeRejected
	FilesystemOutputRuntimeRolledBack
	FilesystemOutputRuntimeAdmitted
	FilesystemOutputRuntimeActive
	FilesystemOutputRuntimeSealed
	FilesystemOutputRuntimeSettled
	FilesystemOutputRuntimeAmbiguous
	FilesystemOutputRuntimeDraining
	FilesystemOutputRuntimeClosed
	FilesystemOutputRuntimeSucceeded
	FilesystemOutputRuntimeReconciled
	FilesystemOutputRuntimeCollision
	FilesystemOutputRuntimeNoChange
	FilesystemOutputRuntimeNeedsAttention
	FilesystemOutputRuntimeIsolatedFailure
	FilesystemOutputRuntimeCleanupPending
)

type FilesystemCheckpointDecision uint8

const (
	FilesystemCheckpointAbsent FilesystemCheckpointDecision = iota + 1
	FilesystemCheckpointExact
	FilesystemCheckpointRevisionConflict
	FilesystemCheckpointOwnershipConflict
	FilesystemCheckpointInvalid
)

type FilesystemOutputNativeLockScope uint8

const FilesystemOutputNativeLockSession FilesystemOutputNativeLockScope = 1

type FilesystemOutputNativeLockMilestone uint8

const (
	FilesystemOutputNativeLockAcquired FilesystemOutputNativeLockMilestone = iota + 1
	FilesystemOutputNativeLockContended
	FilesystemOutputNativeLockAcquireFailed
	FilesystemOutputNativeLockReleased
	FilesystemOutputNativeLockReleaseReportedFailure
)

type FilesystemOutputTrace struct {
	Operation               FilesystemOutputTraceOperation
	DestinationCapabilities outputcap.DestinationCapabilities
	ExecutionMode           outputcap.ExecutionMode
	ReceiveIntentDigest     transfer.ReceiveIntentDigest
	ReceiveOperationID      receivecontract.OperationID
	SessionID               transfer.OutputSessionID
	IdentityScheme          FilesystemOutputIdentitySchemeID
	NativeLockScope         FilesystemOutputNativeLockScope
	NativeLockMilestone     FilesystemOutputNativeLockMilestone
	RootOpenDisposition     FilesystemOutputRootDisposition
	RuntimeComponent        FilesystemOutputRuntimeComponent
	RuntimeOperation        FilesystemOutputRuntimeOperation
	RuntimeDecision         FilesystemOutputRuntimeDecision
	CheckpointDecision      FilesystemCheckpointDecision
	OperationID             uint64
	ClaimID                 uint64
	FaultDomain             uint8
	NormalizedFaultScope    uint8
	NormalizedFaultCode     uint16
	NodeClaimCount          uint64
	DirectoryClaimCount     uint64
	FileClaimCount          uint64
	ActiveFileClaimCount    uint64
	ReservedFileSlotCount   uint64
	DirectoryMetadataBytes  uint64
	CheckpointRecordCount   uint64
	FailureStage            FilesystemOutputFailureStage
	ReconciliationStep      FilesystemCheckpointReconciliationStep
	NativeErrorClass        FilesystemNativeErrorClass
	Failed                  bool
}

type FilesystemOutputTracer interface {
	TraceFilesystemOutput(FilesystemOutputTrace)
}

type FilesystemOutputTraceFunc func(FilesystemOutputTrace)

func (function FilesystemOutputTraceFunc) TraceFilesystemOutput(event FilesystemOutputTrace) {
	if function != nil {
		function(event)
	}
}

type outputSessionIDGenerator interface {
	NewOutputSessionID() (transfer.OutputSessionID, error)
}

// PlatformFactory is the build-tagged native boundary supplied by osfs.
type PlatformFactory func(string, bool) (outputcap.Platform, error)

type Config struct {
	RootPath        string
	CreateRoot      bool
	Tracer          FilesystemOutputTracer
	PlatformFactory PlatformFactory
}

type authorityStage uint8

const (
	authorityStageConstructed authorityStage = iota + 1
	authorityStageBound
	authorityStageLookupHeld
	authorityStageOperationReady
	authorityStageLookupStopped
)

type Authority struct {
	rootPath        string
	createRoot      bool
	sessionIDs      outputSessionIDGenerator
	tracer          FilesystemOutputTracer
	platformFactory PlatformFactory
	random          io.Reader
	now             func() time.Time

	mu          sync.Mutex
	stage       authorityStage
	closed      bool
	destination *destinationauthority.BoundDestination
	binding     destinationauthority.Binding
	mode        outputcap.ExecutionMode
	registry    *checkpointstore.OperationRegistry
	admission   *heldAdmission
	operation   *Operation
}

func New(config Config) (*Authority, error) {
	return &Authority{
		rootPath: config.RootPath, createRoot: config.CreateRoot,
		sessionIDs: cryptographicOutputSessionIDs{}, tracer: config.Tracer,
		platformFactory: config.PlatformFactory, random: rand.Reader, now: time.Now,
		stage: authorityStageConstructed,
	}, nil
}

type cryptographicOutputSessionIDs struct{}

func (cryptographicOutputSessionIDs) NewOutputSessionID() (transfer.OutputSessionID, error) {
	var raw [transfer.OutputSessionIdentityBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return transfer.OutputSessionID{}, err
	}
	return transfer.OutputSessionIDFromBytes(raw[:])
}

func (authority *Authority) trace(event FilesystemOutputTrace) {
	if authority != nil && authority.tracer != nil {
		authority.tracer.TraceFilesystemOutput(event)
	}
}

type NativeDirectTreeReservationKind uint8

const (
	NativeDirectTreeReserved NativeDirectTreeReservationKind = iota + 1
	NativeDirectTreeReopened
	NativeDirectTreeNeedsAttention
)

type NativeDirectTreeReservation struct {
	kind   NativeDirectTreeReservationKind
	intent transfer.ReceiveIntent
}

func (reservation NativeDirectTreeReservation) Kind() NativeDirectTreeReservationKind {
	return reservation.kind
}

func (reservation NativeDirectTreeReservation) ReceiveIntent() (transfer.ReceiveIntent, bool) {
	return reservation.intent, !reservation.intent.IsZero() &&
		(reservation.kind == NativeDirectTreeReserved || reservation.kind == NativeDirectTreeReopened)
}

func (authority *Authority) ReserveDirectTree(
	ctx context.Context,
	selection transfer.SelectionSpec,
	artifact receivecontract.ArtifactSpec,
) (NativeDirectTreeReservation, error) {
	if authority == nil {
		return NativeDirectTreeReservation{}, transfer.ErrInvalidOutputBinding
	}
	authority.mu.Lock()
	staged := authority.stage != authorityStageConstructed || authority.closed
	authority.mu.Unlock()
	if staged {
		return NativeDirectTreeReservation{}, transfer.ErrInvalidOutputBinding
	}
	return authority.reserveNativeDirectTree(ctx, selection, artifact)
}

func (authority *Authority) OpenDirectTree(
	ctx context.Context,
	intent transfer.ReceiveIntent,
) (transfer.DirectTreeSession, error) {
	return authority.openNativeOutput(ctx, intent)
}

func validateOutputCreateAuthority(directory outputcap.Directory) error {
	validator, ok := directory.(outputcap.CreateAuthorityValidator)
	if !ok {
		return nil
	}
	return validator.ValidateCreateAuthority()
}
