package transfer

import (
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/catalog"
)

const (
	maximumCatalogNodeClaims       = catalog.DefaultShareCommittedEntries + 1
	MaximumOpaqueSelectionEvidence = MaxSelectionRuleOverrides * catalog.MaxPathDepth
)

const (
	MaximumRetainedJobFailures      = 4_096
	MaximumRetainedFailurePathBytes = uint64(1) << 20
)

var (
	ErrNodeLedgerBudget              = errors.New("transfer node identity ledger budget exceeded")
	ErrNodeLedgerState               = errors.New("transfer node identity ledger state is invalid")
	ErrOpaqueSelectionEvidenceBudget = errors.New("transfer opaque selection evidence budget exceeded")
)

type opaqueSelectionEvidence struct {
	generation catalog.DirectoryGeneration
	terminal   catalog.PageCommitment
}

type plannedFile struct {
	file              catalog.FileID
	path              string
	expectedSize      uint64
	modified          catalog.ModifiedTime
	parentDirectory   catalog.DirectoryID
	parentGeneration  catalog.DirectoryGeneration
	parentAdmission   DirectoryAdmission
	selectionDecision FileSelectionDecision
}

type transferQueueItemKind uint8

const (
	transferQueueFile transferQueueItemKind = iota + 1
	transferQueueDirectoryFinalization
)

type transferQueueItem struct {
	kind      transferQueueItemKind
	file      plannedFile
	directory OutputDirectory
	enqueued  <-chan struct{}
}

type nodeLedgerCheckpoint int

type nodeIdentityLedger struct {
	claims map[catalog.NodeID]struct{}
	order  []catalog.NodeID
	limit  int
}

func newNodeIdentityLedger(limit int) (nodeIdentityLedger, error) {
	if limit <= 0 {
		return nodeIdentityLedger{}, ErrNodeLedgerState
	}
	return nodeIdentityLedger{
		claims: make(map[catalog.NodeID]struct{}),
		limit:  limit,
	}, nil
}

type jobRun struct {
	job                        *TransferJob
	output                     OutputSession
	failureMu                  sync.Mutex
	directories                []DirectoryJobFailure
	files                      []FileJobFailure
	failurePathBytes           uint64
	omittedDirectories         uint64
	omittedFiles               uint64
	sourceDriftFailure         error
	succeeded                  uint64
	terminationCause           error
	settlementFailure          error
	settlement                 JobSettlement
	admitted                   bool
	needsAttention             bool
	selectionIdentity          SelectionIdentity
	selectionObservation       SelectionObservationV1
	selectionResolutionFailure error
	matchedPaths               map[string]struct{}
	matchedDirectories         map[catalog.DirectoryID]struct{}
	matchedFiles               map[catalog.FileID]struct{}
	unmatchedOpaqueTargets     int
	activeDirectories          map[catalog.DirectoryID]struct{}
	discoveryFailed            bool
	rootGeneration             catalog.DirectoryGeneration
	nodeLedger                 nodeIdentityLedger
	opaqueSelectionEvidence    map[catalog.DirectoryID]opaqueSelectionEvidence
}

func newJobRun(job *TransferJob) (*jobRun, error) {
	if job == nil || job.root.IsZero() {
		return nil, ErrNodeLedgerState
	}
	ledger, err := newNodeIdentityLedger(maximumCatalogNodeClaims)
	if err != nil {
		return nil, err
	}
	run := &jobRun{
		job:                    job,
		matchedPaths:           make(map[string]struct{}),
		matchedDirectories:     make(map[catalog.DirectoryID]struct{}),
		matchedFiles:           make(map[catalog.FileID]struct{}),
		unmatchedOpaqueTargets: job.rules.selectedOpaqueTargetCount(),
		activeDirectories:      make(map[catalog.DirectoryID]struct{}),
		nodeLedger:             ledger,
	}
	if job.rules.requiresOpaqueSelectionSearch() {
		run.opaqueSelectionEvidence = make(map[catalog.DirectoryID]opaqueSelectionEvidence)
	}
	if err := run.claimNode(job.root.NodeID()); err != nil {
		return nil, err
	}
	return run, nil
}

func (r *jobRun) matchSelectedDirectory(directory catalog.DirectoryID) {
	if !r.job.rules.isSelectedDirectoryTarget(directory) {
		return
	}
	if _, matched := r.matchedDirectories[directory]; matched {
		return
	}
	r.matchedDirectories[directory] = struct{}{}
	if r.unmatchedOpaqueTargets > 0 {
		r.unmatchedOpaqueTargets--
	}
}

func (r *jobRun) matchSelectedFile(file catalog.FileID) {
	if !r.job.rules.isSelectedFileTarget(file) {
		return
	}
	if _, matched := r.matchedFiles[file]; matched {
		return
	}
	r.matchedFiles[file] = struct{}{}
	if r.unmatchedOpaqueTargets > 0 {
		r.unmatchedOpaqueTargets--
	}
}

func (r *jobRun) allOpaqueSelectionTargetsMatched() bool {
	return r.job.rules.requiresOpaqueSelectionSearch() && r.unmatchedOpaqueTargets == 0
}

func (r *jobRun) retainOpaqueSelectionEvidence(
	directory catalog.DirectoryID,
	evidence opaqueSelectionEvidence,
) error {
	if directory.IsZero() || evidence.generation.IsZero() || evidence.terminal.IsZero() ||
		r.opaqueSelectionEvidence == nil {
		return NewJobDependencyContractError(ErrNodeLedgerState)
	}
	if retained, exists := r.opaqueSelectionEvidence[directory]; exists {
		if retained != evidence {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		return nil
	}
	if len(r.opaqueSelectionEvidence) >= MaximumOpaqueSelectionEvidence {
		return NewJobResourceBudgetError(ErrOpaqueSelectionEvidenceBudget)
	}
	r.opaqueSelectionEvidence[directory] = evidence
	return nil
}

func (r *jobRun) opaqueEvidence(
	directory catalog.DirectoryID,
) (opaqueSelectionEvidence, bool) {
	evidence, retained := r.opaqueSelectionEvidence[directory]
	return evidence, retained
}

// claimNode records identity before selection rules are applied. Catalog NodeID
// is kind-neutral, so this closes both duplicate-entry and file/directory alias
// attacks before any output authority is granted.
func (r *jobRun) claimNode(identity catalog.NodeID) error {
	return r.nodeLedger.claim(identity)
}

func (ledger *nodeIdentityLedger) claim(identity catalog.NodeID) error {
	if identity.IsZero() {
		return NewSessionFailure(ErrCatalogIdentity)
	}
	if _, exists := ledger.claims[identity]; exists {
		return NewSessionFailure(ErrCatalogIdentity)
	}
	if len(ledger.order) >= ledger.limit {
		// Once the bounded ledger is full, uniqueness is no longer provable for
		// the remaining catalog suffix and discovery must fail closed.
		return NewJobResourceBudgetError(ErrNodeLedgerBudget)
	}
	ledger.claims[identity] = struct{}{}
	ledger.order = append(ledger.order, identity)
	return nil
}

func (r *jobRun) checkpointClaims() nodeLedgerCheckpoint {
	return r.nodeLedger.checkpoint()
}

func (ledger *nodeIdentityLedger) checkpoint() nodeLedgerCheckpoint {
	return nodeLedgerCheckpoint(len(ledger.order))
}

// rollbackClaims releases only an isolated directory suffix. An authenticated
// sibling remains claimed, while a discarded generation cannot exhaust the
// bounded ledger or poison a later independent branch that reuses its NodeID.
func (r *jobRun) rollbackClaims(checkpoint nodeLedgerCheckpoint) error {
	return r.nodeLedger.rollback(checkpoint)
}

func (ledger *nodeIdentityLedger) rollback(checkpoint nodeLedgerCheckpoint) error {
	keep := int(checkpoint)
	if keep < 0 || keep > len(ledger.order) {
		return ErrNodeLedgerState
	}
	for _, identity := range ledger.order[keep:] {
		delete(ledger.claims, identity)
	}
	ledger.order = ledger.order[:keep]
	return nil
}

func (r *jobRun) recordDiscoveryFailure(directory catalog.DirectoryID, path string, err error) error {
	if isJobTerminalError(err) || !isDirectoryDiscoveryFailure(err) {
		return err
	}
	r.job.tracker.failDiscovery()
	r.discoveryFailed = true
	r.recordDirectoryFailure(DirectoryJobFailure{
		DirectoryID: directory, Path: path, Stage: FailureDirectoryDiscovery, Cause: err,
	})
	return nil
}

func isDirectoryDiscoveryFailure(err error) bool {
	return inspectLifecycleError(err).directoryDiscovery
}

func (r *jobRun) recordDirectoryFailure(failure DirectoryJobFailure) {
	r.failureMu.Lock()
	r.retainSourceDriftFailure(failure.Cause)
	if r.reserveFailurePath(failure.Path) {
		r.directories = append(r.directories, failure)
	} else if r.omittedDirectories != ^uint64(0) {
		r.omittedDirectories++
	}
	r.failureMu.Unlock()
}

func (r *jobRun) recordFileFailure(failure FileJobFailure) {
	r.failureMu.Lock()
	r.retainSourceDriftFailure(failure.Cause)
	if r.reserveFailurePath(failure.Path) {
		r.files = append(r.files, failure)
	} else if r.omittedFiles != ^uint64(0) {
		r.omittedFiles++
	}
	r.failureMu.Unlock()
}

func (r *jobRun) reserveFailurePath(path string) bool {
	if len(r.directories)+len(r.files) >= MaximumRetainedJobFailures {
		return false
	}
	pathBytes := uint64(len(path))
	if pathBytes > MaximumRetainedFailurePathBytes-r.failurePathBytes {
		return false
	}
	r.failurePathBytes += pathBytes
	return true
}

func (r *jobRun) retainSourceDriftFailure(cause error) {
	if r.sourceDriftFailure == nil && isSourceDriftFailure(cause) {
		r.sourceDriftFailure = cause
	}
}

func (r *jobRun) failureSnapshot() (
	directories []DirectoryJobFailure,
	files []FileJobFailure,
	omittedDirectories uint64,
	omittedFiles uint64,
	sourceDriftFailure error,
) {
	r.failureMu.Lock()
	defer r.failureMu.Unlock()
	return slices.Clone(r.directories), slices.Clone(r.files), r.omittedDirectories, r.omittedFiles,
		r.sourceDriftFailure
}

func appendOutputPath(parent, name string) (string, error) {
	path := name
	if parent != "" {
		path = strings.Join([]string{parent, name}, "/")
	}
	return catalog.CanonicalPath(path)
}
