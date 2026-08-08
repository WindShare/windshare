package outputruntime

import (
	"context"
	"crypto/rand"
	"io"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

type FilesystemOutputTraceOperation uint8

const (
	TraceFilesystemCertified FilesystemOutputTraceOperation = iota + 1
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

type FilesystemOutputCertificationID string

const (
	FilesystemOutputCertificationLinuxExt4ProcessRestart   FilesystemOutputCertificationID = "linux/ext4/process-restart/v2"
	FilesystemOutputCertificationWindowsNTFSProcessRestart FilesystemOutputCertificationID = "windows/ntfs/process-restart/v1"
)

func filesystemOutputCertificationFromState(certification outputcap.CertificationID) FilesystemOutputCertificationID {
	switch certification {
	case outputcap.CertificationLinuxExt4ProcessRestart:
		return FilesystemOutputCertificationLinuxExt4ProcessRestart
	case outputcap.CertificationWindowsNTFSProcessRestart:
		return FilesystemOutputCertificationWindowsNTFSProcessRestart
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
	FilesystemOutputRuntimeOpenOutput FilesystemOutputRuntimeOperation = iota + 1
	FilesystemOutputRuntimeAcquireIntentLease
	FilesystemOutputRuntimeReconcileCheckpoints
	FilesystemOutputRuntimeAdmitDirectory
	FilesystemOutputRuntimeFinalizeDirectory
	FilesystemOutputRuntimeBeginFile
	FilesystemOutputRuntimeWriteRange
	FilesystemOutputRuntimeCheckpointFile
	FilesystemOutputRuntimeCommitFile
	FilesystemOutputRuntimePauseFile
	FilesystemOutputRuntimeRetireFile
	FilesystemOutputRuntimePauseJob
	FilesystemOutputRuntimeCompleteJob
	FilesystemOutputRuntimeMaterializeDirectory
	FilesystemOutputRuntimeCreateOwnedFile
	FilesystemOutputRuntimeRecoverFile
	FilesystemOutputRuntimePublishFile
	FilesystemOutputRuntimeQuarantineFile
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
	Operation              FilesystemOutputTraceOperation
	IntentDigest           transfer.TransferIntentDigest
	SessionID              transfer.OutputSessionID
	Certification          FilesystemOutputCertificationID
	NativeLockScope        FilesystemOutputNativeLockScope
	NativeLockMilestone    FilesystemOutputNativeLockMilestone
	RootOpenDisposition    FilesystemOutputRootDisposition
	RuntimeComponent       FilesystemOutputRuntimeComponent
	RuntimeOperation       FilesystemOutputRuntimeOperation
	RuntimeDecision        FilesystemOutputRuntimeDecision
	OperationID            uint64
	ClaimID                uint64
	FaultDomain            uint8
	NormalizedFaultScope   uint8
	NormalizedFaultCode    uint16
	NodeClaimCount         uint64
	DirectoryClaimCount    uint64
	FileClaimCount         uint64
	ActiveFileClaimCount   uint64
	ReservedFileSlotCount  uint64
	DirectoryMetadataBytes uint64
	CheckpointRecordCount  uint64
	Failed                 bool
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

const filesystemOutputBackendID = transfer.NativeFilesystemOutputBackendID

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

type Authority struct {
	rootPath        string
	createRoot      bool
	sessionIDs      outputSessionIDGenerator
	tracer          FilesystemOutputTracer
	platformFactory PlatformFactory
	random          io.Reader
}

func New(config Config) (*Authority, error) {
	return &Authority{
		rootPath: config.RootPath, createRoot: config.CreateRoot,
		sessionIDs: cryptographicOutputSessionIDs{}, tracer: config.Tracer,
		platformFactory: config.PlatformFactory, random: rand.Reader,
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

func (authority *Authority) OpenOutput(
	ctx context.Context,
	intent transfer.TransferIntent,
) (transfer.OutputSession, error) {
	return authority.openNativeOutput(ctx, intent)
}

func validateOutputCreateAuthority(directory outputcap.Directory) error {
	validator, ok := directory.(outputcap.CreateAuthorityValidator)
	if !ok {
		return nil
	}
	return validator.ValidateCreateAuthority()
}
