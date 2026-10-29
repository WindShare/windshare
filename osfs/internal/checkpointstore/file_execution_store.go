package checkpointstore

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/fileexecution"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

const (
	ownedAnchorSuffix = ".anchor"
	ownedStageSuffix  = ".stage"
)

// StagePolicy describes permission inheritance independently from crash cleanup.
type StagePolicy uint8

const (
	PrivateStage StagePolicy = iota
	InheritFinalParent
)

// FileExecutionStore binds the native file engine to the physical checkpoint
// repository. Its logical authority state is shared through the operation
// lease, so separate adapters cannot both observe an absent lineage and install
// competing objects.
type FileExecutionStore struct {
	mu sync.Mutex

	repository  *Repository
	stagePolicy StagePolicy
	authority   *fileExecutionAuthority
}

var (
	_ fileexecution.CheckpointRepository = (*FileExecutionStore)(nil)
	_ fileexecution.Platform             = (*FileExecutionStore)(nil)
)

func NewFileExecutionStore(repository *Repository) (*FileExecutionStore, error) {
	return NewFileExecutionStoreWithStagePolicy(repository, PrivateStage)
}

func NewFileExecutionStoreWithStagePolicy(
	repository *Repository,
	stagePolicy StagePolicy,
) (*FileExecutionStore, error) {
	return newFileExecutionStore(repository, stagePolicy)
}

// NewFreshFileExecutionStore deliberately skips repository enumeration. It is
// valid only for a newly published ordinary operation whose private namespace
// cannot contain an earlier file record.
func NewFreshFileExecutionStore(repository *Repository) (*FileExecutionStore, error) {
	return NewFreshFileExecutionStoreWithStagePolicy(repository, PrivateStage)
}

func NewFreshFileExecutionStoreWithStagePolicy(
	repository *Repository,
	stagePolicy StagePolicy,
) (*FileExecutionStore, error) {
	store, err := newFileExecutionStore(repository, stagePolicy)
	if err != nil {
		return nil, err
	}
	store.authority.mu.Lock()
	defer store.authority.mu.Unlock()
	if len(store.authority.physical) != 0 || len(store.authority.attention) != 0 {
		return nil, codedError(ErrorUnsafeInstall, "open fresh file execution store", checkpointmodel.ErrRecordBinding)
	}
	store.authority.inventoryComplete = true
	return store, nil
}

func newFileExecutionStore(
	repository *Repository,
	stagePolicy StagePolicy,
) (*FileExecutionStore, error) {
	if stagePolicy > InheritFinalParent || repository == nil || repository.records == nil || repository.anchors == nil ||
		repository.stages == nil || repository.authority == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return &FileExecutionStore{
		repository: repository, stagePolicy: stagePolicy, authority: repository.authority,
	}, nil
}

const maximumCachedFileRecords = 256

// The task lease excludes other writers. A bounded read-through cache avoids
// rebuilding a directory-wide manifest before the first resumed range.
func (store *FileExecutionStore) loadLineageLocked(spec checkpointmodel.CheckpointLineageSpec) (checkpointmodel.Record, bool, error) {
	if err := store.authority.requireSettled(); err != nil {
		return checkpointmodel.Record{}, false, err
	}
	id, err := checkpointmodel.DeriveCheckpointLineageID(spec)
	if err != nil {
		return checkpointmodel.Record{}, false, err
	}
	if record, found := store.authority.physical[checkpointmodel.RecordID(id)]; found {
		return record, true, nil
	}
	if len(store.authority.physical) >= maximumCachedFileRecords {
		if err := store.authority.rebuild(nil, nil); err != nil {
			return checkpointmodel.Record{}, false, err
		}
		store.authority.inventoryComplete = false
	}
	record, err := store.repository.Reopen(checkpointmodel.RecordID(id))
	if errors.Is(err, fs.ErrNotExist) {
		return checkpointmodel.Record{}, false, nil
	}
	if err != nil {
		return checkpointmodel.Record{}, false, err
	}
	actual, err := record.CheckpointLineageSpec()
	if err != nil || !checkpointmodel.SameCheckpointLineageSpec(actual, spec) {
		return checkpointmodel.Record{}, false, checkpointmodel.ErrRecordBinding
	}
	if err := store.authority.add(record); err != nil {
		return checkpointmodel.Record{}, false, err
	}
	return record, true, nil
}

func (store *FileExecutionStore) LookupDeliveredRecord(ctx context.Context, spec checkpointmodel.CheckpointLineageSpec) (checkpointmodel.Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return checkpointmodel.Record{}, false, err
	}
	store.authority.mu.Lock()
	defer store.authority.mu.Unlock()
	record, found, err := store.loadLineageLocked(spec)
	if ErrorCodeFor(err) == ErrorCorruptRecord {
		return checkpointmodel.Record{}, false, nil
	}
	if err != nil {
		return checkpointmodel.Record{}, false, err
	}
	return record, found && record.Phase() == checkpointmodel.PhasePublished && record.CommitState() == checkpointmodel.CommitPublished, nil
}

func (store *FileExecutionStore) inventoryLocked() error {
	if err := store.authority.requireSettled(); err != nil {
		return err
	}
	if store.authority.inventoryComplete {
		return nil
	}
	snapshot, err := store.repository.Reconcile(func(record checkpointmodel.Record) (bool, error) {
		if _, active := store.authority.physical[record.RecordID()]; active {
			return false, nil
		}
		return store.candidateDurable(record)
	})
	if err != nil {
		return err
	}
	for _, record := range snapshot.Records() {
		if cached, found := store.authority.physical[record.RecordID()]; found &&
			!bytes.Equal(cached.CanonicalBytes(), record.CanonicalBytes()) {
			store.authority.markUnsettled()
			return checkpointmodel.ErrRecordBinding
		}
	}
	if err := store.authority.rebuild(snapshot.Records(), snapshot.Attention()); err != nil {
		return err
	}
	for _, image := range snapshot.candidates {
		store.authority.candidates[image.reference()] = image
	}
	store.authority.reclaimedCandidates += snapshot.ReclaimedCandidateCount()
	store.authority.inventoryComplete = true
	return nil
}

// Inventory is reserved for explicit management and cleanup. Ordinary transfer
// loads each logical file on demand, so unrelated state cannot delay its start.
// Attention describes observed repository anomalies; a failed inventory returns
// an error because an incomplete observation cannot establish record corruption.
func (store *FileExecutionStore) Inventory() ([]FileExecutionLineageSlot, []Attention, error) {
	if store == nil || store.authority == nil {
		return nil, nil, transfer.ErrInvalidOutputBinding
	}
	store.authority.mu.Lock()
	defer store.authority.mu.Unlock()
	if err := store.inventoryLocked(); err != nil {
		return nil, nil, err
	}
	return store.authority.lineageSnapshot(), slices.Clone(store.authority.attention), nil
}

// CleanupOwned retires every authenticated physical record, including records
// in conflict slots and uninstalled images that cannot authorize recovery.
// Shared object claims are retired once; each image survives until its owned
// data is durably removed, regardless of the fixed record's generation.
func (store *FileExecutionStore) CleanupOwned(ctx context.Context) error {
	if store == nil || store.authority == nil || ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	store.authority.mu.Lock()
	defer store.authority.mu.Unlock()
	if err := store.inventoryLocked(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.authority.requireSettled(); err != nil {
		return codedError(ErrorStateIO, "cleanup unsettled ordinary file state", err)
	}
	images := make([]checkpointImage, 0, len(store.authority.physical)+len(store.authority.candidates))
	for _, record := range store.authority.physical {
		images = append(images, fixedCheckpointImage(record))
	}
	candidateReferences := make(map[string]struct{}, len(store.authority.candidates))
	for reference, image := range store.authority.candidates {
		images = append(images, image)
		candidateReferences[reference] = struct{}{}
	}
	slices.SortFunc(images, func(left, right checkpointImage) int {
		if order := compareRecordID(left.record, right.record); order != 0 {
			return order
		}
		return strings.Compare(left.name, right.name)
	})
	retiredObjects := make(map[checkpointmodel.ObjectID]struct{})
	var cleanupErr error
	for start := 0; start < len(images); start += maximumCleanupBatchRecords {
		end := min(start+maximumCleanupBatchRecords, len(images))
		if err := store.cleanupBatch(ctx, images[start:end], retiredObjects); err != nil {
			cleanupErr = repositoryError("cleanup ordinary file state", err)
			break
		}
	}
	// Remove resolved diagnostics once per cleanup, including completed batches
	// before a failure. Scanning attention per image would make recovery quadratic.
	store.authority.attention = slices.DeleteFunc(store.authority.attention, func(attention Attention) bool {
		_, known := candidateReferences[attention.Reference()]
		_, retained := store.authority.candidates[attention.Reference()]
		return known && !retained
	})
	if cleanupErr != nil {
		return cleanupErr
	}
	// Unknown auxiliary objects survive, but do not retain independently owned
	// data after the user has discarded the operation.
	if len(store.authority.attention) != 0 {
		return codedError(ErrorUnsafeInstall, "cleanup ordinary file state", outputcap.ErrUnsafeNamespace)
	}
	return nil
}

func (store *FileExecutionStore) ReclaimedCandidateCount() uint64 {
	if store == nil || store.authority == nil {
		return 0
	}
	store.authority.mu.Lock()
	defer store.authority.mu.Unlock()
	return store.authority.reclaimedCandidates
}

func (store *FileExecutionStore) RecordCount() uint64 {
	if store == nil || store.authority == nil {
		return 0
	}
	store.authority.mu.Lock()
	defer store.authority.mu.Unlock()
	return uint64(len(store.authority.physical))
}

func (store *FileExecutionStore) Lookup(
	ctx context.Context,
	key fileexecution.CheckpointKey,
) (fileexecution.CheckpointResolution, error) {
	if store == nil || store.authority == nil || ctx == nil {
		return fileexecution.CheckpointResolution{}, dependencyBoundaryError(
			"lookup file execution checkpoint", transfer.ErrInvalidOutputBinding,
		)
	}
	if err := ctx.Err(); err != nil {
		return fileexecution.CheckpointResolution{}, err
	}
	spec, err := key.CheckpointLineageSpec()
	if err != nil {
		return fileexecution.CheckpointResolution{}, dependencyBoundaryError("derive checkpoint lineage", err)
	}
	store.authority.mu.Lock()
	defer store.authority.mu.Unlock()
	if _, _, err := store.loadLineageLocked(spec); err != nil {
		if ErrorCodeFor(err) == ErrorCorruptRecord {
			return fileexecution.ResolveCheckpoint(checkpointmodel.CheckpointLineageDecisionInvalid, checkpointmodel.Record{})
		}
		return fileexecution.CheckpointResolution{}, err
	}
	decision, record, err := store.authority.classify(spec, key.CheckpointLineageRequest())
	if err != nil {
		if errors.Is(err, errFileExecutionAuthorityUnsettled) {
			return fileexecution.CheckpointResolution{}, codedError(
				ErrorStateIO, "classify unsettled file checkpoint authority", err,
			)
		}
		return fileexecution.CheckpointResolution{}, codedError(ErrorCorruptRecord, "classify file checkpoint lineage", err)
	}
	if decision == checkpointmodel.CheckpointLineageDecisionExact && !checkpointKeyMatchesRecord(key, record) {
		return fileexecution.CheckpointResolution{}, codedError(
			ErrorCorruptRecord, "lookup file execution checkpoint", checkpointmodel.ErrRecordBinding,
		)
	}
	return fileexecution.ResolveCheckpoint(decision, record)
}

func (store *FileExecutionStore) InstallInitial(
	ctx context.Context,
	key fileexecution.CheckpointKey,
	candidate checkpointmodel.Record,
) (fileexecution.InitialCheckpointObservation, error) {
	if store == nil || store.authority == nil || ctx == nil || !candidate.Valid() ||
		!checkpointmodel.InitialCandidate(candidate) || !checkpointKeyMatchesRecord(key, candidate) {
		return fileexecution.InitialCheckpointObservation{}, dependencyBoundaryError(
			"install initial file checkpoint", transfer.ErrInvalidOutputBinding,
		)
	}
	if err := ctx.Err(); err != nil {
		return fileexecution.InitialCheckpointObservation{}, err
	}
	spec, err := key.CheckpointLineageSpec()
	if err != nil {
		return fileexecution.InitialCheckpointObservation{}, dependencyBoundaryError("derive initial checkpoint lineage", err)
	}
	return store.installInitial(ctx, spec, key.CheckpointLineageRequest(), candidate)
}

func (store *FileExecutionStore) installInitialRecord(
	ctx context.Context,
	candidate checkpointmodel.Record,
) (fileexecution.InitialCheckpointObservation, error) {
	spec, err := candidate.CheckpointLineageSpec()
	if err != nil {
		return fileexecution.InitialCheckpointObservation{}, err
	}
	return store.installInitial(ctx, spec, checkpointmodel.CheckpointLineageRequest{
		FileRevision: candidate.FileRevision(), ExactSize: candidate.ExactSize(),
	}, candidate)
}

func (store *FileExecutionStore) installInitial(
	ctx context.Context,
	spec checkpointmodel.CheckpointLineageSpec,
	request checkpointmodel.CheckpointLineageRequest,
	candidate checkpointmodel.Record,
) (fileexecution.InitialCheckpointObservation, error) {
	if store == nil || store.authority == nil || ctx == nil || !checkpointmodel.InitialCandidate(candidate) {
		return fileexecution.InitialCheckpointObservation{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return fileexecution.InitialCheckpointObservation{}, err
	}
	store.authority.mu.Lock()
	defer store.authority.mu.Unlock()
	if _, _, err := store.loadLineageLocked(spec); err != nil {
		return fileexecution.InitialCheckpointObservation{}, err
	}
	decision, selected, err := store.authority.classify(spec, request)
	if err != nil {
		return fileexecution.InitialCheckpointObservation{}, initialAuthorityError(err)
	}
	if decision != checkpointmodel.CheckpointLineageDecisionAbsent {
		resolution, resolveErr := fileexecution.ResolveCheckpoint(decision, selected)
		observation, observeErr := fileexecution.ObserveInitialCheckpoint(resolution, false)
		return observation, errors.Join(resolveErr, observeErr)
	}
	if err := store.authority.admitInitial(candidate); err != nil {
		return fileexecution.InitialCheckpointObservation{}, initialAuthorityError(err)
	}

	observed, operationErr := store.repository.Create(candidate)
	if operationErr != nil {
		observed, observationErr := store.repository.Reopen(candidate.RecordID())
		// An exact reread diagnoses an ambiguous create, but only a successful
		// installation operation proves directory durability. Freeze this lease's
		// authority until reconciliation establishes the physical reality.
		store.authority.markUnsettled()
		if observationErr == nil && !bytes.Equal(observed.CanonicalBytes(), candidate.CanonicalBytes()) {
			observationErr = checkpointmodel.ErrRecordBinding
		}
		return fileexecution.InitialCheckpointObservation{}, transferfault.ReduceBoundaryErrors(
			ctx, operationErr, observationErr,
		)
	}
	if !bytes.Equal(observed.CanonicalBytes(), candidate.CanonicalBytes()) {
		store.authority.markUnsettled()
		return fileexecution.InitialCheckpointObservation{}, transferfault.ReduceBoundaryErrors(
			ctx, checkpointmodel.ErrRecordBinding,
		)
	}
	if err := store.authority.add(observed); err != nil {
		store.authority.markUnsettled()
		return fileexecution.InitialCheckpointObservation{}, transferfault.ReduceBoundaryErrors(ctx, err)
	}
	decision, selected, err = store.authority.classify(spec, request)
	if err != nil || decision != checkpointmodel.CheckpointLineageDecisionExact ||
		selected.RecordID() != candidate.RecordID() {
		store.authority.markUnsettled()
		return fileexecution.InitialCheckpointObservation{}, transferfault.ReduceBoundaryErrors(
			ctx, err, checkpointmodel.ErrRecordBinding,
		)
	}
	resolution, err := fileexecution.ResolveCheckpoint(decision, selected)
	if err != nil {
		store.authority.markUnsettled()
		return fileexecution.InitialCheckpointObservation{}, err
	}
	observation, err := fileexecution.ObserveInitialCheckpoint(resolution, true)
	if err != nil {
		store.authority.markUnsettled()
	}
	return observation, err
}

func initialAuthorityError(err error) error {
	switch {
	case errors.Is(err, fileexecution.ErrCheckpointObjectClaimed),
		errors.Is(err, fileexecution.ErrCheckpointRecordCapacity):
		return err
	case errors.Is(err, errFileExecutionAuthorityUnsettled):
		return codedError(ErrorStateIO, "admit initial checkpoint through unsettled authority", err)
	default:
		return codedError(ErrorCorruptRecord, "admit initial file checkpoint", err)
	}
}

func (store *FileExecutionStore) Replace(
	ctx context.Context,
	previous checkpointmodel.Record,
	next checkpointmodel.Record,
) (fileexecution.CheckpointObservation, error) {
	if store == nil || store.authority == nil || ctx == nil || !previous.Valid() || !next.Valid() ||
		previous.RecordID() != next.RecordID() {
		return fileexecution.CheckpointObservation{}, dependencyBoundaryError(
			"replace file execution checkpoint", transfer.ErrInvalidOutputBinding,
		)
	}
	if err := ctx.Err(); err != nil {
		return fileexecution.CheckpointObservation{}, err
	}
	spec, err := previous.CheckpointLineageSpec()
	if err != nil {
		return fileexecution.CheckpointObservation{}, err
	}
	store.authority.mu.Lock()
	defer store.authority.mu.Unlock()
	if _, _, err := store.loadLineageLocked(spec); err != nil {
		return fileexecution.CheckpointObservation{}, err
	}
	decision, selected, err := store.authority.classify(spec, checkpointmodel.CheckpointLineageRequest{
		FileRevision: previous.FileRevision(), ExactSize: previous.ExactSize(),
	})
	if errors.Is(err, errFileExecutionAuthorityUnsettled) {
		return fileexecution.CheckpointObservation{}, codedError(
			ErrorStateIO, "replace through unsettled file checkpoint authority", err,
		)
	}
	if err != nil || decision != checkpointmodel.CheckpointLineageDecisionExact ||
		selected.RecordID() != previous.RecordID() {
		return fileexecution.CheckpointObservation{}, codedError(
			ErrorUnsafeInstall, "replace conflicted file checkpoint", errors.Join(err, checkpointmodel.ErrRecordBinding),
		)
	}

	observed, operationErr := store.repository.Replace(previous, next)
	if operationErr != nil {
		observed, observationErr := store.repository.Reopen(next.RecordID())
		store.authority.markUnsettled()
		if observationErr == nil &&
			!bytes.Equal(observed.CanonicalBytes(), next.CanonicalBytes()) &&
			!bytes.Equal(observed.CanonicalBytes(), previous.CanonicalBytes()) {
			observationErr = checkpointmodel.ErrRecordBinding
		}
		return fileexecution.CheckpointObservation{}, transferfault.ReduceBoundaryErrors(
			ctx, operationErr, observationErr,
		)
	}
	if bytes.Equal(observed.CanonicalBytes(), next.CanonicalBytes()) {
		if err := store.authority.replace(previous, observed); err != nil {
			store.authority.markUnsettled()
			return fileexecution.CheckpointObservation{}, transferfault.ReduceBoundaryErrors(ctx, err)
		}
	} else if !bytes.Equal(observed.CanonicalBytes(), previous.CanonicalBytes()) {
		store.authority.markUnsettled()
		return fileexecution.CheckpointObservation{}, transferfault.ReduceBoundaryErrors(
			ctx, checkpointmodel.ErrRecordBinding,
		)
	}
	observation, observeErr := fileexecution.ObservedCheckpoint(observed)
	if observeErr != nil {
		store.authority.markUnsettled()
	}
	return observation, transferfault.ReduceBoundaryErrors(ctx, observeErr)
}

func checkpointKeyMatchesRecord(key fileexecution.CheckpointKey, record checkpointmodel.Record) bool {
	return record.Valid() && record.OperationID() == key.OperationID() &&
		record.ReceiveIntentDigest() == key.ReceiveIntentDigest() &&
		record.MaterializationBindingDigest() == key.MaterializationBindingDigest() &&
		record.FileID() == key.FileID() && record.FileRevision() == key.FileRevision() &&
		record.CanonicalPath() == key.CanonicalPath() && record.ExactSize() == key.ExactSize() &&
		record.MaterializerKind() == key.MaterializerKind() && record.AuthorityRef() == key.AuthorityRef()
}

func compareRecordID(left, right checkpointmodel.Record) int {
	return bytes.Compare(left.RecordID().Bytes(), right.RecordID().Bytes())
}
