package outputruntime

import (
	"crypto/rand"
	"io"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type TraceOperation uint8

const (
	TraceCheckpointReconciled TraceOperation = iota + 1
	TraceRuntimeDecision
)

type RuntimeComponent uint8

const (
	RuntimeSession RuntimeComponent = iota + 1
	RuntimeDirectory
	RuntimeFile
	RuntimeCheckpoint
)

type RuntimeOperation uint8

const (
	RuntimeReconcileCheckpoints RuntimeOperation = iota + 1
	RuntimeAdmitDirectory
	RuntimeFinalizeDirectory
	RuntimeBeginFile
	RuntimeWriteRange
	RuntimeCheckpointFile
	RuntimeCommitFile
	RuntimePauseFile
	RuntimeRetireFile
	RuntimePauseTree
	RuntimeFinalizeTree
	RuntimeMaterializeDirectory
	RuntimeCreateOwnedFile
	RuntimeRecoverFile
	RuntimePublishFile
	RuntimeQuarantineFile
	RuntimeAdmitDestination
	RuntimeFirstWrite
	RuntimeCleanup
	RuntimeDeliveryReceipt
	RuntimeMapName
	RuntimeSelectOperation
)

type RuntimeDecision uint8

const (
	RuntimeValidated RuntimeDecision = iota + 1
	RuntimeReserved
	RuntimeCoalesced
	RuntimeRejected
	RuntimeRolledBack
	RuntimeAdmitted
	RuntimeActive
	RuntimeSealed
	RuntimeSettled
	RuntimeAmbiguous
	RuntimeDraining
	RuntimeClosed
	RuntimeSucceeded
	RuntimeReconciled
	RuntimeCollision
	RuntimeNoChange
	RuntimeNeedsAttention
	RuntimeIsolatedFailure
	RuntimeCleanupPending
	RuntimeSelectionMismatch
)

type CheckpointDecision uint8

const (
	CheckpointAbsent CheckpointDecision = iota + 1
	CheckpointExact
	CheckpointRevisionConflict
	CheckpointOwnershipConflict
	CheckpointInvalid
	CheckpointMetadataMatch
	CheckpointReuseStateBlocked
	CheckpointRevisionEvidenceUnavailable
)

// Trace records detached native output milestones and capability decisions.
// Observations never carry handles or grant durable recovery authority.
type Trace struct {
	LogicalPath                       string
	PhysicalPath                      string
	NameMappingAttempt                uint32
	NameMappingReused                 bool
	FileID                            catalog.FileID
	Operation                         TraceOperation
	DestinationCapabilities           outputcap.DestinationCapabilities
	DestinationBindingID              outputcap.DestinationBindingID
	ReceiveIntentDigest               transfer.ReceiveIntentDigest
	ReceiveOperationID                receivecontract.OperationID
	SessionID                         transfer.OutputSessionID
	IdentityScheme                    outputcap.IdentitySchemeID
	RuntimeComponent                  RuntimeComponent
	RuntimeOperation                  RuntimeOperation
	RuntimeDecision                   RuntimeDecision
	PreviousRevision                  content.FileRevision
	CurrentRevision                   content.FileRevision
	RecoveryReason                    content.RevisionComparisonReason
	ReusedBytes                       uint64
	RetainedBytes                     uint64
	CheckpointDecision                CheckpointDecision
	OperationID                       uint64
	ClaimID                           uint64
	WriteOffset                       uint64
	WriteLength                       uint64
	CheckpointAttentionCode           string
	CheckpointAttentionCount          uint64
	CheckpointReclaimedCandidateCount uint64
	FaultDomain                       uint8
	NormalizedFaultScope              uint8
	NormalizedFaultCode               uint16
	NodeClaimCount                    uint64
	DirectoryClaimCount               uint64
	FileClaimCount                    uint64
	ActiveFileClaimCount              uint64
	ReservedFileSlotCount             uint64
	DirectoryMetadataBytes            uint64
	CheckpointRecordCount             uint64
	FailureStage                      FailureStage
	ReconciliationStep                CheckpointReconciliationStep
	NativeErrorClass                  NativeErrorClass
	Failed                            bool
}

type Tracer interface {
	TraceOutput(Trace)
}

type TraceFunc func(Trace)

func (function TraceFunc) TraceOutput(event Trace) {
	if function != nil {
		function(event)
	}
}

// PlatformFactory is the build-tagged native boundary supplied by osfs.
type PlatformFactory func(string, bool) (outputcap.Platform, error)

type Config struct {
	RootPath        string
	CreateRoot      bool
	Diagnostics     diagnostics.Sink
	Tracer          Tracer
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
	diagnostics     diagnostics.Sink
	tracer          Tracer
	platformFactory PlatformFactory
	random          io.Reader
	now             func() time.Time

	mu          sync.Mutex
	stage       authorityStage
	closed      bool
	destination *destinationauthority.BoundDestination
	binding     destinationauthority.Binding
	registry    *checkpointstore.OperationRegistry
	admission   *heldAdmission
	operation   *Operation
}

func New(config Config) (*Authority, error) {
	return &Authority{
		rootPath: config.RootPath, createRoot: config.CreateRoot,
		diagnostics: config.Diagnostics, tracer: config.Tracer,
		platformFactory: config.PlatformFactory, random: rand.Reader, now: time.Now,
		stage: authorityStageConstructed,
	}, nil
}

func (authority *Authority) trace(event Trace) {
	if authority == nil {
		return
	}
	if authority.tracer != nil {
		authority.tracer.TraceOutput(event)
	}
	if authority.diagnostics.Enabled() {
		authority.diagnostics.Record(event)
	}
}
