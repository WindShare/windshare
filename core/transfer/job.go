package transfer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
)

var (
	ErrInvalidTransferJob    = errors.New("transfer job configuration is invalid")
	ErrTransferJobRun        = errors.New("transfer job may only run once")
	ErrCatalogIdentity       = errors.New("catalog snapshot does not match the requested share and directory")
	ErrRevisionIdentity      = errors.New("opened revision does not match the selected catalog file")
	ErrOutputContract        = errors.New("output session violated its file transaction contract")
	ErrCatalogEntriesOmitted = errors.New("catalog directory omitted children")
	ErrCatalogLeaseContract  = errors.New("catalog reader returned no release callback")
)

type JobOutcome uint8

const (
	JobSucceeded JobOutcome = iota + 1
	JobCompletedWithErrors
	JobAborted
)

type FailureStage uint8

const (
	FailureDirectoryDiscovery FailureStage = iota + 1
	FailureDirectoryOutput
	FailureRevisionOpen
	FailureRevisionIdentity
	FailureBlockTransfer
	FailureFileOutput
	FailureLeaseRelease
)

type DirectoryJobFailure struct {
	DirectoryID catalog.DirectoryID
	Path        string
	Stage       FailureStage
	Err         error
}

type FileJobFailure struct {
	FileID catalog.FileID
	Path   string
	Stage  FailureStage
	Err    error
}

type JobResult struct {
	Outcome        JobOutcome
	Measure        SelectionMeasure
	Directories    []DirectoryJobFailure
	Files          []FileJobFailure
	SucceededFiles uint64
	AbortCause     error
}

type CatalogReader interface {
	// Implementations must be safe for concurrent calls. The release callback is
	// mandatory even when err is a typed directory failure, because authenticated
	// failures also consume bounded cache memory. Returned immutable values remain
	// valid after release; release relinquishes source-cache accounting only.
	AcquireDirectory(context.Context, catalog.DirectoryID) (catalog.DirectorySnapshot, func(), error)
}

type DirectoryDiscoveryFailure interface {
	error
	DirectoryFailure()
}

type OpenedRevision struct {
	LeaseID    content.LeaseID
	Descriptor content.FileRevisionDescriptor
}

func NewOpenedRevision(lease content.LeaseID, descriptor content.FileRevisionDescriptor) (OpenedRevision, error) {
	if lease.IsZero() || descriptor.ShareInstance().IsZero() || descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() {
		return OpenedRevision{}, ErrRevisionIdentity
	}
	return OpenedRevision{LeaseID: lease, Descriptor: descriptor}, nil
}

type RevisionClient interface {
	OpenRevision(context.Context, catalog.FileID) (OpenedRevision, error)
	ReleaseRevision(context.Context, content.LeaseID) error
}

type RangeReader interface {
	ReadRange(context.Context, content.LeaseID, content.FileRevisionDescriptor, content.Range, RangeSink) error
}

type TransferJobConfig struct {
	ShareInstance catalog.ShareInstance
	SyntheticRoot catalog.DirectoryID
	Rules         SelectionRules
	Catalog       CatalogReader
	Revisions     RevisionClient
	Blocks        RangeReader
	Output        OutputSession
}

type TransferJob struct {
	share              catalog.ShareInstance
	root               catalog.DirectoryID
	rules              SelectionRules
	catalog            *catalogReplayReader
	measurementCatalog *catalogReplayReader
	catalogReplay      *catalogReplay
	revisions          RevisionClient
	blocks             RangeReader
	output             OutputSession
	tracker            selectionTracker
	admission          selectionTracker

	mu      sync.Mutex
	started bool
}

func NewTransferJob(config TransferJobConfig) (*TransferJob, error) {
	if config.ShareInstance.IsZero() || config.SyntheticRoot.IsZero() || !config.Rules.validSnapshot() ||
		config.Catalog == nil || config.Revisions == nil || config.Blocks == nil || config.Output == nil {
		return nil, ErrInvalidTransferJob
	}
	capabilities := config.Output.Capabilities()
	if _, err := NewOutputCapabilities(capabilities); err != nil || config.Output.BackendID() == "" || config.Output.SessionID().IsZero() {
		return nil, ErrInvalidTransferJob
	}
	replay := newCatalogReplay(config.Catalog)
	return &TransferJob{
		share: config.ShareInstance, root: config.SyntheticRoot, rules: config.Rules,
		catalog: replay.reader(catalogReplayExecution), measurementCatalog: replay.reader(catalogReplayMeasurement),
		catalogReplay: replay, revisions: config.Revisions, blocks: config.Blocks, output: config.Output,
		tracker: newSelectionTracker(), admission: newSelectionTracker(),
	}, nil
}

func (j *TransferJob) Measure() SelectionMeasure { return j.tracker.snapshot() }

// SelectionMeasures replays the latest job-scoped measurement and then emits
// coalesced monotonic updates. A caller must subscribe before Run when an early
// Large or terminal Small transition controls transport admission.
func (j *TransferJob) SelectionMeasures() <-chan SelectionMeasure { return j.admission.Updates() }

func (j *TransferJob) Run(ctx context.Context) JobResult {
	j.mu.Lock()
	if j.started {
		j.mu.Unlock()
		return JobResult{Outcome: JobAborted, Measure: j.Measure(), AbortCause: ErrTransferJobRun}
	}
	j.started = true
	j.mu.Unlock()

	runContext, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	measurementDone := make(chan error, 1)
	go func() {
		measure, err := measureSelection(runContext, SelectionMeasurementConfig{
			ShareInstance: j.share, SyntheticRoot: j.root, Rules: j.rules, Catalog: j.measurementCatalog,
		}, j.admission.replace)
		j.admission.replace(measure)
		j.admission.closeUpdates()
		if err != nil {
			cancel(err)
		}
		j.measurementCatalog.Close()
		measurementDone <- err
	}()

	state := &jobRun{
		job: j, claims: newSelectionIdentityClaims(j.root),
		matchedPaths:       make(map[string]struct{}),
		matchedDirectories: make(map[catalog.DirectoryID]struct{}), matchedFiles: make(map[catalog.FileID]struct{}),
	}
	var executionErr error
	if j.rules.HasSelection() {
		rootSelected := j.rules.DirectorySelectedAt(j.root, "", j.rules.DefaultSelected())
		executionErr = state.walkDirectory(runContext, j.root, newRootOutputFrame(), rootSelected)
		if executionErr == nil && !state.discoveryFailed {
			executionErr = j.rules.missingTargetsError(
				state.matchedPaths, state.matchedDirectories, state.matchedFiles,
			)
		}
		if executionErr != nil {
			j.tracker.failDiscovery()
			cancel(executionErr)
		}
	}
	j.tracker.finishDiscovery()
	j.tracker.closeUpdates()
	j.catalog.Close()
	measurementErr := <-measurementDone
	// Both readers are closed before Wait, so no later Load can race a
	// WaitGroup.Add. Joining source loads also makes cancellation causes and
	// cache-lease release observable before the job reports completion.
	j.catalogReplay.Wait()
	if executionErr != nil || measurementErr != nil {
		state.abortCause = context.Cause(runContext)
		if state.abortCause == nil {
			state.abortCause = errors.Join(executionErr, measurementErr)
		}
	}
	return state.finish(ctx)
}

func (r *jobRun) transferFile(
	ctx context.Context,
	frame *directoryOutputFrame,
	path string,
	entry catalog.Entry,
) error {
	file, _ := entry.FileID()
	opened, ready, err := r.openSelectedRevision(ctx, file, path, entry)
	if err != nil || !ready {
		return err
	}
	outputAvailable, err := frame.ensure(ctx, r)
	if err != nil {
		releaseErr := r.job.revisions.ReleaseRevision(context.WithoutCancel(ctx), opened.LeaseID)
		return errors.Join(err, releaseErr)
	}
	if !outputAvailable {
		return r.releaseLease(ctx, file, path, opened.LeaseID)
	}
	transaction, durable, ready, err := r.beginOutputFile(ctx, file, path, entry, opened)
	if err != nil || !ready {
		return err
	}
	completed, err := r.transferMissingRanges(ctx, file, path, opened, transaction, durable)
	if err != nil || !completed {
		return err
	}
	return r.commitTransferredFile(ctx, file, path, opened, transaction)
}

func (r *jobRun) openSelectedRevision(ctx context.Context, file catalog.FileID, path string, entry catalog.Entry) (OpenedRevision, bool, error) {
	opened, err := r.job.revisions.OpenRevision(ctx, file)
	if err != nil {
		if isJobTerminalError(err) {
			return OpenedRevision{}, false, err
		}
		r.files = append(r.files, FileJobFailure{FileID: file, Path: path, Stage: FailureRevisionOpen, Err: err})
		return OpenedRevision{}, false, nil
	}
	if err := validateOpenedFile(r.job.share, entry, opened); err != nil {
		if releaseErr := r.releaseLease(ctx, file, path, opened.LeaseID); releaseErr != nil {
			return OpenedRevision{}, false, releaseErr
		}
		r.files = append(r.files, FileJobFailure{FileID: file, Path: path, Stage: FailureRevisionIdentity, Err: err})
		return OpenedRevision{}, false, nil
	}
	return opened, true, nil
}

func (r *jobRun) beginOutputFile(ctx context.Context, file catalog.FileID, path string, entry catalog.Entry, opened OpenedRevision) (FileTransaction, VerifiedDurableRanges, bool, error) {
	descriptor := opened.Descriptor
	transaction, durable, err := r.job.output.BeginFile(ctx, OutputFile{
		Path: path, ExpectedSize: entry.ExpectedSize(), Descriptor: descriptor,
	})
	if err != nil {
		if releaseErr := r.releaseLease(ctx, file, path, opened.LeaseID); releaseErr != nil {
			return nil, VerifiedDurableRanges{}, false, releaseErr
		}
		if isJobTerminalError(err) || outputFailureRequiresJobAbort(err, r.job.output.Capabilities()) {
			return nil, VerifiedDurableRanges{}, false, err
		}
		r.files = append(r.files, FileJobFailure{FileID: file, Path: path, Stage: FailureFileOutput, Err: err})
		return nil, VerifiedDurableRanges{}, false, nil
	}
	if err := validateOutputTransaction(r.job.output, path, descriptor, transaction, durable); err != nil {
		contractErr := NewOutputSessionError(err, true)
		if transaction == nil {
			releaseErr := r.job.revisions.ReleaseRevision(context.WithoutCancel(ctx), opened.LeaseID)
			return nil, VerifiedDurableRanges{}, false, errors.Join(contractErr, releaseErr)
		}
		return nil, VerifiedDurableRanges{}, false, r.abortTransferredFile(ctx, file, path, opened, transaction, FailureFileOutput, contractErr)
	}
	return transaction, durable, true, nil
}

func (r *jobRun) transferMissingRanges(ctx context.Context, file catalog.FileID, path string, opened OpenedRevision, transaction FileTransaction, durable VerifiedDurableRanges) (bool, error) {
	descriptor := opened.Descriptor
	missing, err := MissingRanges(descriptor.ExactSize(), durable.Ranges())
	if err != nil {
		return false, r.abortTransferredFile(ctx, file, path, opened, transaction, FailureFileOutput, err)
	}
	for _, requested := range splitAtBlockBoundaries(missing, descriptor.Geometry()) {
		checkpoint, completed, err := r.transferRange(ctx, file, path, opened, transaction, durable, requested)
		if err != nil || !completed {
			return false, err
		}
		durable = checkpoint
	}
	if !RangesCoverFile(descriptor.ExactSize(), durable.Ranges()) {
		contractErr := NewOutputSessionError(ErrOutputContract, true)
		return false, r.abortTransferredFile(ctx, file, path, opened, transaction, FailureFileOutput, contractErr)
	}
	return true, nil
}

func (r *jobRun) transferRange(ctx context.Context, file catalog.FileID, path string, opened OpenedRevision, transaction FileTransaction, durable VerifiedDurableRanges, requested content.Range) (VerifiedDurableRanges, bool, error) {
	err := r.job.blocks.ReadRange(ctx, opened.LeaseID, opened.Descriptor, requested, transaction)
	if err != nil {
		return VerifiedDurableRanges{}, false, r.handleRangeReadFailure(ctx, file, path, opened, transaction, err)
	}
	checkpoint, err := transaction.Checkpoint(ctx)
	if err != nil {
		return VerifiedDurableRanges{}, false, r.abortTransferredFile(ctx, file, path, opened, transaction, FailureFileOutput, err)
	}
	if checkpoint.Binding() != transaction.Binding() || checkpoint.Generation() <= durable.Generation() ||
		!rangeContains(checkpoint.Ranges(), requested) || !rangesContain(checkpoint.Ranges(), durable.Ranges()) {
		contractErr := NewOutputSessionError(ErrOutputContract, true)
		return VerifiedDurableRanges{}, false, r.abortTransferredFile(ctx, file, path, opened, transaction, FailureFileOutput, contractErr)
	}
	return checkpoint, true, nil
}

func rangesContain(available, required content.RangeSet) bool {
	availableRanges := available.Ranges()
	availableIndex := 0
	for _, requiredRange := range required.Ranges() {
		for availableIndex < len(availableRanges) && availableRanges[availableIndex].End < requiredRange.End {
			availableIndex++
		}
		if availableIndex == len(availableRanges) || availableRanges[availableIndex].Offset > requiredRange.Offset ||
			availableRanges[availableIndex].End < requiredRange.End {
			return false
		}
	}
	return true
}

func (r *jobRun) handleRangeReadFailure(ctx context.Context, file catalog.FileID, path string, opened OpenedRevision, transaction FileTransaction, cause error) error {
	if isJobTerminalError(cause) {
		_, abortErr := transaction.Abort(context.WithoutCancel(ctx), cause)
		releaseErr := r.job.revisions.ReleaseRevision(context.WithoutCancel(ctx), opened.LeaseID)
		return errors.Join(cause, abortErr, releaseErr)
	}
	if invalidator, ok := r.job.blocks.(interface {
		InvalidateRevision(catalog.FileID, content.FileRevision)
	}); ok && (errors.Is(cause, content.ErrRevisionDrift) || errors.Is(cause, ErrBlockInvalidated)) {
		invalidator.InvalidateRevision(file, opened.Descriptor.FileRevision())
	}
	return r.abortTransferredFile(ctx, file, path, opened, transaction, FailureBlockTransfer, cause)
}

func (r *jobRun) commitTransferredFile(ctx context.Context, file catalog.FileID, path string, opened OpenedRevision, transaction FileTransaction) error {
	if err := transaction.Commit(ctx); err != nil {
		return r.abortTransferredFile(ctx, file, path, opened, transaction, FailureFileOutput, err)
	}
	if releaseErr := r.job.revisions.ReleaseRevision(context.WithoutCancel(ctx), opened.LeaseID); releaseErr != nil {
		if isJobTerminalError(releaseErr) {
			return releaseErr
		}
		r.files = append(r.files, FileJobFailure{FileID: file, Path: path, Stage: FailureLeaseRelease, Err: releaseErr})
	}
	r.succeeded++
	return nil
}

func validateOpenedFile(share catalog.ShareInstance, entry catalog.Entry, opened OpenedRevision) error {
	file, isFile := entry.FileID()
	descriptor := opened.Descriptor
	if !isFile || opened.LeaseID.IsZero() || descriptor.ShareInstance() != share || descriptor.FileID() != file ||
		descriptor.FileRevision().IsZero() || descriptor.ExactSize() != entry.ExpectedSize() {
		return ErrRevisionIdentity
	}
	if entry.ModifiedTime().Present() && descriptor.ModifiedTime() != entry.ModifiedTime() {
		return ErrRevisionIdentity
	}
	return nil
}

func validateOutputTransaction(output OutputSession, path string, descriptor content.FileRevisionDescriptor, transaction FileTransaction, durable VerifiedDurableRanges) error {
	if transaction == nil {
		return ErrOutputContract
	}
	binding := transaction.Binding()
	if binding.BackendID() != output.BackendID() || binding.OutputSessionID() != output.SessionID() ||
		binding.ShareInstance() != descriptor.ShareInstance() || binding.FileID() != descriptor.FileID() ||
		binding.FileRevision() != descriptor.FileRevision() || binding.ExactSize() != descriptor.ExactSize() ||
		binding.Locator().IsZero() || durable.Binding() != binding {
		return ErrOutputContract
	}
	if binding.Locator().Kind() == OutputPathLocator {
		locator, err := NewPathOutputLocator(path)
		if err != nil || binding.Locator() != locator {
			return ErrOutputContract
		}
	}
	return nil
}

func splitAtBlockBoundaries(ranges content.RangeSet, geometry content.FileGeometry) []content.Range {
	result := make([]content.Range, 0)
	chunk := uint64(geometry.ChunkSize())
	for _, current := range ranges.Ranges() {
		for offset := current.Offset; offset < current.End; {
			next := min(current.End, ((offset/chunk)+1)*chunk)
			result = append(result, content.Range{Offset: offset, End: next})
			offset = next
		}
	}
	return result
}

func rangeContains(ranges content.RangeSet, target content.Range) bool {
	for _, current := range ranges.Ranges() {
		if current.Offset <= target.Offset && current.End >= target.End {
			return true
		}
	}
	return false
}

func (r *jobRun) abortTransferredFile(ctx context.Context, file catalog.FileID, path string, opened OpenedRevision, transaction FileTransaction, stage FailureStage, cause error) error {
	disposition, abortErr := transaction.Abort(context.WithoutCancel(ctx), cause)
	releaseErr := r.job.revisions.ReleaseRevision(context.WithoutCancel(ctx), opened.LeaseID)
	combined := errors.Join(cause, abortErr, releaseErr)
	validDisposition := disposition >= FileAbortIsolated && disposition <= FileAbortRequiresJobAbort
	if !validDisposition {
		combined = errors.Join(combined, NewOutputSessionError(ErrOutputContract, true))
	}
	requiresOutputAbort := !validDisposition || disposition == FileAbortRequiresJobAbort ||
		outputFailureExplicitlyRequiresJobAbort(cause) || outputFailureExplicitlyRequiresJobAbort(abortErr) ||
		(disposition != FileAbortSkippedBeforeStart && !r.job.output.Capabilities().FileFailureIsolation)
	if isJobTerminalError(cause) || isJobTerminalError(abortErr) || isJobTerminalError(releaseErr) || requiresOutputAbort {
		return combined
	}
	r.files = append(r.files, FileJobFailure{FileID: file, Path: path, Stage: stage, Err: combined})
	return nil
}

func (r *jobRun) releaseLease(ctx context.Context, file catalog.FileID, path string, lease content.LeaseID) error {
	if err := r.job.revisions.ReleaseRevision(context.WithoutCancel(ctx), lease); err != nil {
		if isJobTerminalError(err) {
			return err
		}
		r.files = append(r.files, FileJobFailure{FileID: file, Path: path, Stage: FailureLeaseRelease, Err: err})
	}
	return nil
}

func (r *jobRun) finish(ctx context.Context) JobResult {
	outcome := r.finishOutput(ctx)
	return JobResult{
		Outcome: outcome, Measure: r.job.Measure(), Directories: slices.Clone(r.directories), Files: slices.Clone(r.files),
		SucceededFiles: r.succeeded, AbortCause: r.abortCause,
	}
}

func (r *jobRun) finishOutput(ctx context.Context) JobOutcome {
	if cause := r.terminalCause(ctx); cause != nil {
		r.abortOutput(ctx, cause)
		return JobAborted
	}
	outcome := JobSucceeded
	if len(r.directories) != 0 || len(r.files) != 0 {
		outcome = JobCompletedWithErrors
	}
	if err := r.job.output.FinishJob(ctx, outcome); err != nil {
		r.abortOutput(ctx, err)
		return JobAborted
	}
	return outcome
}

func (r *jobRun) terminalCause(ctx context.Context) error {
	if r.abortCause != nil {
		return r.abortCause
	}
	return context.Cause(ctx)
}

func (r *jobRun) abortOutput(ctx context.Context, cause error) {
	r.abortCause = cause
	if err := r.job.output.AbortJob(context.WithoutCancel(ctx), cause); err != nil {
		r.abortCause = errors.Join(cause, err)
	}
}

type SessionFailureError struct{ cause error }

func NewSessionFailure(cause error) error {
	if cause == nil {
		cause = errors.New("protocol session failed")
	}
	return &SessionFailureError{cause: cause}
}

func (e *SessionFailureError) Error() string   { return fmt.Sprintf("transfer session: %v", e.cause) }
func (e *SessionFailureError) Unwrap() error   { return e.cause }
func (e *SessionFailureError) SessionFailure() {}

func isSessionFailure(err error) bool {
	var scoped interface{ SessionFailure() }
	return errors.As(err, &scoped) || errors.Is(err, protocolsession.ErrSessionTerminated) ||
		errors.Is(err, protocolsession.ErrPeerSessionTerminal) || errors.Is(err, protocolsession.ErrWriterTerminal) ||
		errors.Is(err, protocolsession.ErrWriterStopped) || errors.Is(err, ErrLaneClosed)
}

func IsSessionFailure(err error) bool { return isSessionFailure(err) }

// JobResourceBudgetError terminates one transfer because a local, bounded
// resource policy was exhausted. Keeping this distinct from SessionFailureError
// prevents local capacity from being misreported as peer identity compromise.
type JobResourceBudgetError struct{ cause error }

func NewJobResourceBudgetError(cause error) error {
	if cause == nil {
		cause = errors.New("transfer job resource budget exceeded")
	}
	return &JobResourceBudgetError{cause: cause}
}

func (e *JobResourceBudgetError) Error() string {
	return fmt.Sprintf("transfer job resource budget: %v", e.cause)
}
func (e *JobResourceBudgetError) Unwrap() error { return e.cause }
func (e *JobResourceBudgetError) JobFatal()     {}

// JobDependencyContractError identifies a local collaborator contract breach.
// It is job-fatal but must not be attributed to the authenticated peer session.
type JobDependencyContractError struct{ cause error }

func NewJobDependencyContractError(cause error) error {
	if cause == nil {
		cause = errors.New("transfer job dependency contract violated")
	}
	return &JobDependencyContractError{cause: cause}
}

func (e *JobDependencyContractError) Error() string {
	return fmt.Sprintf("transfer job dependency contract: %v", e.cause)
}
func (e *JobDependencyContractError) Unwrap() error { return e.cause }
func (e *JobDependencyContractError) JobFatal()     {}

func isJobFatal(err error) bool {
	var fatal interface{ JobFatal() }
	return errors.As(err, &fatal)
}

func isJobTerminalError(err error) bool {
	return isSessionFailure(err) || isJobFatal(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
