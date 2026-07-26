package transfer

import (
	"context"
	"errors"
	"fmt"
)

var ErrInvalidOutputSettlement = errors.New("transfer output settlement is invalid")

type OutputFaultScope uint8

const (
	OutputFaultFile OutputFaultScope = iota + 1
	OutputFaultSession
	OutputFaultRoot
)

type OutputFaultCode uint16

const (
	OutputFaultStateIO OutputFaultCode = iota + 1
	OutputFaultStateCorrupt
	OutputFaultOwnership
	OutputFaultNamespaceUnsafe
	OutputFaultUnsupportedFilesystem
	OutputFaultContract
)

// OutputFault makes settlement failure scope machine-readable while preserving
// the original backend cause for diagnostics.
type OutputFault struct {
	scope OutputFaultScope
	code  OutputFaultCode
	cause error
}

func NewOutputFault(scope OutputFaultScope, code OutputFaultCode, cause error) error {
	if scope < OutputFaultFile || scope > OutputFaultRoot ||
		code < OutputFaultStateIO || code > OutputFaultContract {
		return ErrInvalidOutputSettlement
	}
	if cause == nil {
		cause = errors.New("output settlement failed")
	}
	return &OutputFault{scope: scope, code: code, cause: cause}
}

func (fault *OutputFault) Error() string {
	return fmt.Sprintf("output fault scope=%d code=%d: %v", fault.scope, fault.code, fault.cause)
}
func (fault *OutputFault) Unwrap() error           { return fault.cause }
func (fault *OutputFault) Scope() OutputFaultScope { return fault.scope }
func (fault *OutputFault) Code() OutputFaultCode   { return fault.code }
func (fault *OutputFault) RequiresJobPause() bool {
	// A raw session-scoped namespace rejection can happen before an output
	// session exists. Runtime namespace failures carry an OutputSessionError
	// that explicitly requires pausing the already admitted session.
	return fault.scope != OutputFaultFile &&
		!(fault.scope == OutputFaultSession && fault.code == OutputFaultNamespaceUnsafe)
}

func outputContractFault(cause error) error {
	if cause == nil || errors.Is(cause, ErrOutputContract) {
		cause = ErrOutputContract
	} else {
		cause = errors.Join(ErrOutputContract, cause)
	}
	return NewOutputFault(OutputFaultSession, OutputFaultContract, cause)
}

func validateSettlementFailure(err error) error {
	if err == nil {
		return nil
	}
	var fault *OutputFault
	if errors.As(err, &fault) && fault.Scope() >= OutputFaultFile && fault.Scope() <= OutputFaultRoot &&
		fault.Code() >= OutputFaultStateIO && fault.Code() <= OutputFaultContract {
		return err
	}
	return outputContractFault(err)
}

// FileSettlementKind is durable output state, not an error classification.
// Keeping collision and quarantine out of the error channel lets a job continue
// other files without guessing backend policy from wrapped error text.
type FileSettlementKind uint8

const (
	FilePublished FileSettlementKind = iota + 1
	FilePaused
	FileRetired
	FileCollision
	FilePublishBlocked
	FileQuarantined
)

type FileSettlement struct {
	kind             FileSettlementKind
	target           OutputFileTarget
	binding          OutputFileBinding
	hasBinding       bool
	checkpoint       VerifiedDurableRanges
	hasCheckpoint    bool
	stateRef         OutputStateRef
	quarantineReason QuarantineReason
}

func NewCollisionFileSettlement(target OutputFileTarget) (FileSettlement, error) {
	if !target.valid() {
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
	return FileSettlement{kind: FileCollision, target: target}, nil
}

func (s FileSettlement) Kind() FileSettlementKind { return s.kind }
func (s FileSettlement) Target() OutputFileTarget { return s.target }

func NewRetiredFileSettlement(binding OutputFileBinding) (FileSettlement, error) {
	if !binding.valid() {
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
	return FileSettlement{
		kind: FileRetired, target: binding.Target(), binding: binding, hasBinding: true,
	}, nil
}

func NewVerifiedFileSettlement(
	kind FileSettlementKind,
	checkpoint VerifiedDurableRanges,
) (FileSettlement, error) {
	if kind != FilePublished && kind != FilePaused && kind != FilePublishBlocked ||
		!checkpoint.Binding().valid() {
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
	if (kind == FilePublished || kind == FilePublishBlocked) &&
		!RangesCoverFile(checkpoint.Binding().ExactSize(), checkpoint.Ranges()) {
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
	return FileSettlement{
		kind: kind, target: checkpoint.Binding().Target(), binding: checkpoint.Binding(), hasBinding: true,
		checkpoint: checkpoint, hasCheckpoint: true,
	}, nil
}

func (s FileSettlement) VerifiedCheckpoint() (VerifiedDurableRanges, bool) {
	return s.checkpoint, s.hasCheckpoint
}

func (s FileSettlement) OutputBinding() (OutputFileBinding, bool) {
	return s.binding, s.hasBinding
}

type OutputStateRef struct {
	session OutputSessionID
	locator OutputLocatorDigest
}

func NewOutputStateRef(session OutputSessionID, locator OutputLocatorDigest) (OutputStateRef, error) {
	if session.IsZero() || locator == (OutputLocatorDigest{}) {
		return OutputStateRef{}, ErrInvalidOutputSettlement
	}
	return OutputStateRef{session: session, locator: locator}, nil
}

func (reference OutputStateRef) OutputSessionID() OutputSessionID   { return reference.session }
func (reference OutputStateRef) LocatorDigest() OutputLocatorDigest { return reference.locator }
func (reference OutputStateRef) IsZero() bool {
	return reference.session.IsZero() || reference.locator == (OutputLocatorDigest{})
}

type QuarantineReason uint16

const (
	QuarantineStateCorrupt QuarantineReason = iota + 1
	QuarantineOwnershipMismatch
	QuarantinePublicationAmbiguous
	QuarantineRetirementMismatch
)

func NewImmediateQuarantinedFileSettlement(
	target OutputFileTarget,
	reference OutputStateRef,
	reason QuarantineReason,
) (FileSettlement, error) {
	if !validQuarantineSettlement(target, reference, reason) {
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
	return FileSettlement{
		kind: FileQuarantined, target: target, stateRef: reference, quarantineReason: reason,
	}, nil
}

func NewTransactionQuarantinedFileSettlement(
	binding OutputFileBinding,
	reference OutputStateRef,
	reason QuarantineReason,
) (FileSettlement, error) {
	if !binding.valid() || !validQuarantineSettlement(binding.Target(), reference, reason) {
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
	return FileSettlement{
		kind: FileQuarantined, target: binding.Target(), binding: binding, hasBinding: true,
		stateRef: reference, quarantineReason: reason,
	}, nil
}

func validQuarantineSettlement(
	target OutputFileTarget,
	reference OutputStateRef,
	reason QuarantineReason,
) bool {
	return target.valid() && !reference.IsZero() &&
		reference.OutputSessionID() == target.OutputSessionID() &&
		reference.LocatorDigest() == target.Locator().Digest() &&
		reason >= QuarantineStateCorrupt && reason <= QuarantineRetirementMismatch
}

func (s FileSettlement) Quarantine() (OutputStateRef, QuarantineReason, bool) {
	return s.stateRef, s.quarantineReason, s.kind == FileQuarantined && !s.stateRef.IsZero() && s.quarantineReason != 0
}

func (kind FileSettlementKind) valid() bool {
	return kind >= FilePublished && kind <= FileQuarantined
}

func (settlement FileSettlement) valid() bool {
	switch settlement.kind {
	case FileCollision:
		return settlement.target.valid() && !settlement.hasBinding && !settlement.hasCheckpoint &&
			settlement.stateRef.IsZero() && settlement.quarantineReason == 0
	case FileRetired:
		return settlement.target.valid() && settlement.hasBinding && settlement.binding.valid() &&
			settlement.binding.Target() == settlement.target && !settlement.hasCheckpoint &&
			settlement.stateRef.IsZero() && settlement.quarantineReason == 0
	case FilePublished, FilePaused, FilePublishBlocked:
		checkpoint, ok := settlement.VerifiedCheckpoint()
		binding, bound := settlement.OutputBinding()
		if !ok || !bound || !binding.valid() || checkpoint.Binding() != binding ||
			settlement.target != binding.Target() || !settlement.stateRef.IsZero() || settlement.quarantineReason != 0 {
			return false
		}
		return (settlement.kind == FilePaused) || RangesCoverFile(checkpoint.Binding().ExactSize(), checkpoint.Ranges())
	case FileQuarantined:
		reference, reason, ok := settlement.Quarantine()
		if !ok || !validQuarantineSettlement(settlement.target, reference, reason) || settlement.hasCheckpoint {
			return false
		}
		return !settlement.hasBinding || settlement.binding.valid() && settlement.binding.Target() == settlement.target
	default:
		return false
	}
}

func (settlement FileSettlement) matchesTarget(target OutputFileTarget) bool {
	return settlement.valid() && target.valid() && settlement.Target() == target
}

func (settlement FileSettlement) matchesBinding(binding OutputFileBinding) bool {
	if !settlement.matchesTarget(binding.Target()) || !binding.valid() {
		return false
	}
	switch settlement.Kind() {
	case FilePublished, FilePaused, FilePublishBlocked:
		checkpoint, _ := settlement.VerifiedCheckpoint()
		return checkpoint.Binding() == binding
	case FileQuarantined, FileRetired:
		settledBinding, ok := settlement.OutputBinding()
		return ok && settledBinding == binding
	default:
		return false
	}
}

type FilePauseReason uint8

const (
	FilePauseInterrupted FilePauseReason = iota + 1
	FilePauseShutdown
	FilePauseTransportFailure
	FilePauseSessionFailure
	FilePauseOutputFailure
)

func (reason FilePauseReason) valid() bool {
	return reason >= FilePauseInterrupted && reason <= FilePauseOutputFailure
}

type FileRetireReason uint8

const (
	FileRetireIsolatedPermanentSourceFailure FileRetireReason = iota + 1
	FileRetireInvalidatedRevision
	FileRetireExplicitPolicySkip
)

func (reason FileRetireReason) valid() bool {
	return reason >= FileRetireIsolatedPermanentSourceFailure && reason <= FileRetireExplicitPolicySkip
}

type JobPauseReason uint8

const (
	JobPauseInterrupted JobPauseReason = iota + 1
	JobPauseShutdown
	JobPauseTransportFailure
	JobPauseSessionFailure
	JobPauseOutputFailure
)

func (reason JobPauseReason) valid() bool {
	return reason >= JobPauseInterrupted && reason <= JobPauseOutputFailure
}

type JobSettlementKind uint8

const (
	JobClosed JobSettlementKind = iota + 1
	JobPaused
	JobPausedNeedsAttention
)

type JobSettlement struct {
	kind JobSettlementKind
}

func NewJobSettlement(kind JobSettlementKind) (JobSettlement, error) {
	if !kind.valid() {
		return JobSettlement{}, ErrInvalidOutputSettlement
	}
	return JobSettlement{kind: kind}, nil
}

func (s JobSettlement) Kind() JobSettlementKind { return s.kind }

func (kind JobSettlementKind) valid() bool {
	return kind >= JobClosed && kind <= JobPausedNeedsAttention
}

type FileTransaction interface {
	Binding() OutputFileBinding
	WriteRange(context.Context, uint64, []byte) error
	Checkpoint(context.Context) (VerifiedDurableRanges, error)
	Commit(context.Context) (FileSettlement, error)
	Pause(context.Context, FilePauseReason) (FileSettlement, error)
	Retire(context.Context, FileRetireReason) (FileSettlement, error)
}

type fileStartKind uint8

const (
	fileStartTransaction fileStartKind = iota + 1
	fileStartSettlement
)

// FileStart is a sum type. An immediate settlement never exposes a transaction,
// so a caller cannot accidentally act on an already terminal result. FileRetired
// is valid here only when a backend deterministically finishes a persisted
// retirement before returning from BeginFile.
type FileStart struct {
	kind        fileStartKind
	transaction FileTransaction
	durable     VerifiedDurableRanges
	settlement  FileSettlement
}

func NewFileTransactionStart(transaction FileTransaction, durable VerifiedDurableRanges) (FileStart, error) {
	if transaction == nil || !transaction.Binding().valid() || durable.Binding() != transaction.Binding() {
		return FileStart{}, ErrInvalidOutputSettlement
	}
	return FileStart{kind: fileStartTransaction, transaction: transaction, durable: durable}, nil
}

func NewFileSettlementStart(settlement FileSettlement) (FileStart, error) {
	switch settlement.Kind() {
	case FilePublished, FileCollision, FilePublishBlocked, FileQuarantined, FileRetired:
		if !settlement.valid() {
			return FileStart{}, ErrInvalidOutputSettlement
		}
		return FileStart{kind: fileStartSettlement, settlement: settlement}, nil
	default:
		return FileStart{}, ErrInvalidOutputSettlement
	}
}

func (start FileStart) Transaction() (FileTransaction, VerifiedDurableRanges, bool) {
	if start.kind != fileStartTransaction || start.transaction == nil {
		return nil, VerifiedDurableRanges{}, false
	}
	return start.transaction, start.durable, true
}

func (start FileStart) ImmediateSettlement() (FileSettlement, bool) {
	return start.settlement, start.kind == fileStartSettlement && start.settlement.valid()
}

func (start FileStart) valid() bool {
	if transaction, durable, ok := start.Transaction(); ok {
		return transaction.Binding().valid() && durable.Binding() == transaction.Binding()
	}
	_, ok := start.ImmediateSettlement()
	return ok
}

// OutputAuthority is the pre-session boundary. It receives only a terminal,
// canonically bound selection, prepares and validates every selected parent,
// and returns a durable session only after admission is complete.
type OutputAuthority interface {
	OpenSelection(context.Context, OutputSelection) (OutputSession, error)
}

type OutputAuthorityFunc func(context.Context, OutputSelection) (OutputSession, error)

func (function OutputAuthorityFunc) OpenSelection(
	ctx context.Context,
	selection OutputSelection,
) (OutputSession, error) {
	if function == nil {
		return nil, ErrInvalidOutputBinding
	}
	return function(ctx, selection)
}

// OutputSession exists only after the exact frozen selection has been admitted.
// A pre-open failure therefore cannot accidentally receive session settlement.
type OutputSession interface {
	BackendID() OutputBackendID
	SessionID() OutputSessionID
	Capabilities() OutputCapabilities
	FinalizeDirectory(context.Context, OutputDirectory) error
	BeginFile(context.Context, OutputFile) (FileStart, error)
	PauseJob(context.Context, JobPauseReason) (JobSettlement, error)
	CompleteJob(context.Context, JobOutcome) (JobSettlement, error)
}
