package checkpointstore

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

const (
	ownedAnchorSuffix = ".anchor"
	ownedStageSuffix  = ".stage"
)

// FileExecutionStore binds the native file engine to the physical checkpoint
// repository. Its logical authority state is shared through the operation
// lease, so separate adapters cannot both observe an absent lineage and install
// competing objects.
type FileExecutionStore struct {
	mu sync.Mutex

	repository *Repository
	profile    checkpointmodel.LiveCleanupNativeProfile
	authority  *fileExecutionAuthority
}

var (
	_ fileexecution.CheckpointRepository = (*FileExecutionStore)(nil)
	_ fileexecution.Platform             = (*FileExecutionStore)(nil)
)

func NewFileExecutionStore(repository *Repository) (*FileExecutionStore, error) {
	return NewFileExecutionStoreWithProfile(repository, 0)
}

func NewFileExecutionStoreWithProfile(
	repository *Repository,
	profile checkpointmodel.LiveCleanupNativeProfile,
) (*FileExecutionStore, error) {
	return newFileExecutionStore(repository, profile, true)
}

// NewFreshFileExecutionStore deliberately skips repository enumeration. It is
// valid only for a newly published ordinary operation whose private namespace
// cannot contain an earlier file record.
func NewFreshFileExecutionStore(repository *Repository) (*FileExecutionStore, error) {
	return NewFreshFileExecutionStoreWithProfile(repository, 0)
}

func NewFreshFileExecutionStoreWithProfile(
	repository *Repository,
	profile checkpointmodel.LiveCleanupNativeProfile,
) (*FileExecutionStore, error) {
	return newFileExecutionStore(repository, profile, false)
}

func newFileExecutionStore(
	repository *Repository,
	profile checkpointmodel.LiveCleanupNativeProfile,
	reconcile bool,
) (*FileExecutionStore, error) {
	if repository == nil || repository.records == nil || repository.anchors == nil ||
		repository.stages == nil || repository.authority == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	store := &FileExecutionStore{
		repository: repository, profile: profile, authority: repository.authority,
	}
	store.authority.mu.Lock()
	defer store.authority.mu.Unlock()
	if !reconcile {
		if len(store.authority.physical) != 0 || len(store.authority.attention) != 0 {
			return nil, codedError(ErrorUnsafeInstall, "open fresh file execution store", checkpointmodel.ErrRecordBinding)
		}
		return store, nil
	}
	snapshot, err := repository.Reconcile(store.candidateDurable)
	if err != nil {
		return nil, err
	}
	// Physical candidate/committed reconciliation is complete before any logical
	// grouping. Corrupt and foreign images remain bounded inert attention.
	if err := store.authority.rebuild(snapshot.Records(), snapshot.Attention()); err != nil {
		return nil, codedError(ErrorCorruptRecord, "index reconciled file checkpoints", err)
	}
	return store, nil
}

// Snapshot retains the physical inventory for exact cleanup and low-level
// diagnostics. Logical consumers must use LineageSnapshot so conflicts never
// acquire range authority through iteration order.
func (store *FileExecutionStore) Snapshot() (records []checkpointmodel.Record, attention []Attention) {
	if store == nil || store.authority == nil {
		return nil, nil
	}
	store.authority.mu.Lock()
	defer store.authority.mu.Unlock()
	records = make([]checkpointmodel.Record, 0, len(store.authority.physical))
	for _, record := range store.authority.physical {
		records = append(records, record)
	}
	slices.SortFunc(records, compareRecordID)
	return records, slices.Clone(store.authority.attention)
}

func (store *FileExecutionStore) LineageSnapshot() (
	slots []FileExecutionLineageSlot,
	attention []Attention,
) {
	if store == nil || store.authority == nil {
		return nil, nil
	}
	store.authority.mu.Lock()
	defer store.authority.mu.Unlock()
	return store.authority.lineageSnapshot(), slices.Clone(store.authority.attention)
}

// CleanupOwned retires every authenticated physical record, including records
// in conflict slots. Shared object claims are retired once and records are never
// discarded merely to manufacture a selected lineage.
func (store *FileExecutionStore) CleanupOwned(ctx context.Context) error {
	if store == nil || store.authority == nil || ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	store.authority.mu.Lock()
	defer store.authority.mu.Unlock()
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.authority.requireSettled(); err != nil {
		return codedError(ErrorStateIO, "cleanup unsettled ordinary file state", err)
	}
	if len(store.authority.attention) != 0 {
		return codedError(ErrorUnsafeInstall, "cleanup ordinary file state", checkpointmodel.ErrRecordRecovery)
	}
	records := make([]checkpointmodel.Record, 0, len(store.authority.physical))
	for _, record := range store.authority.physical {
		records = append(records, record)
	}
	slices.SortFunc(records, compareRecordID)
	retiredObjects := make(map[checkpointmodel.ObjectID]struct{})
	for _, record := range records {
		if _, retired := retiredObjects[record.OwnedObjectID()]; !retired {
			for _, step := range []fileexecution.RetirementStep{
				fileexecution.RetirementRemoveStage,
				fileexecution.RetirementSyncStageNamespace,
				fileexecution.RetirementRemoveAnchor,
				fileexecution.RetirementSyncAnchorNamespace,
			} {
				observation, err := store.applyRetirementLocked(record.OwnedObjectID(), step)
				if err != nil || !observation.ValidForCleanup(record.OwnedObjectID()) {
					return errors.Join(err, checkpointmodel.ErrRecordRecovery)
				}
			}
			retiredObjects[record.OwnedObjectID()] = struct{}{}
		}
		if err := store.repository.Remove(record); err != nil {
			return err
		}
		if err := store.authority.remove(record); err != nil {
			return codedError(ErrorCorruptRecord, "remove file checkpoint authority", err)
		}
	}
	return nil
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

	operationErr := store.repository.Create(candidate)
	observed, observationErr := store.repository.Reopen(candidate.RecordID())
	if operationErr != nil {
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
	if observationErr != nil {
		store.authority.markUnsettled()
		return fileexecution.InitialCheckpointObservation{}, transferfault.ReduceBoundaryErrors(
			ctx, observationErr,
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

	operationErr := store.repository.Replace(previous, next)
	observed, observationErr := store.repository.Reopen(next.RecordID())
	if operationErr != nil {
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
	if observationErr != nil {
		store.authority.markUnsettled()
		return fileexecution.CheckpointObservation{}, transferfault.ReduceBoundaryErrors(
			ctx, observationErr,
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
