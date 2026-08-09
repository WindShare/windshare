package directoryauthority

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

type materializationState uint8

const (
	materializationPending materializationState = iota + 1
	materializationReady
	materializationAmbiguous
)

type finalizationState uint8

const (
	finalizationUnstarted finalizationState = iota
	finalizationPending
	finalizationSettled
	finalizationAmbiguous
)

type claimRecord struct {
	claim           directoryClaim
	state           materializationState
	materialization directoryMaterialization
	retained        outputcap.Directory

	snapshotOnce sync.Once
	snapshot     parentNamespaceIndex
	snapshotErr  error

	finalizationState finalizationState
	finalization      directoryFinalization
}

// Authority owns only live native directory capabilities and path-local indexes.
// Output claim state, locator ownership, node identity, and catalog-global NodeID
// authority deliberately remain in their respective callers.
type Authority struct {
	platform        Platform
	snapshotter     ParentNamespaceSnapshotter
	snapshotLimit   int
	rootDisposition outputcap.RootOpenDisposition
	trace           func(TraceEvent)

	gate sync.RWMutex
	mu   sync.Mutex

	closed      bool
	rootClaimID ClaimID
	claims      map[ClaimID]*claimRecord
	admissions  map[string]ClaimID
	reservedKey string
}

var (
	_ outputsession.LocatorCanonicalizer = (*Authority)(nil)
	_ outputsession.DirectoryExecutor    = (*Authority)(nil)
)

func New(platform Platform, config Config) (*Authority, error) {
	if platform == nil || config.ParentSnapshotEntryLimit < 0 {
		return nil, ErrInvalidConfiguration
	}
	limit := config.ParentSnapshotEntryLimit
	if limit == 0 {
		limit = DefaultParentSnapshotEntryLimit
	}
	if limit <= 0 || limit > catalog.MaxDirectoryEntries {
		return nil, ErrInvalidConfiguration
	}
	disposition := platform.RootOpenDisposition()
	if disposition != outputcap.CallerProvidedContainer && disposition != outputcap.AuthorityCreatedRoot {
		return nil, ErrInvalidConfiguration
	}
	reservedKey, err := platform.CanonicalComponentKey(reservedControlComponent)
	if err != nil || reservedKey == "" {
		return nil, errors.Join(ErrInvalidConfiguration, err)
	}
	snapshotter := config.Snapshotter
	if snapshotter == nil {
		snapshotter = capabilityNameSnapshotter{}
	}
	return &Authority{
		platform: platform, snapshotter: snapshotter, snapshotLimit: limit,
		rootDisposition: disposition, trace: config.Trace,
		claims: make(map[ClaimID]*claimRecord), admissions: make(map[string]ClaimID), reservedKey: reservedKey,
	}, nil
}

func (authority *Authority) newDirectoryClaim(
	id ClaimID,
	parentID ClaimID,
	locator locatorKey,
	modified catalog.ModifiedTime,
	admissions ...transfer.DirectoryAdmission,
) (directoryClaim, error) {
	if len(admissions) > 1 {
		return directoryClaim{}, ErrInvalidClaim
	}
	var admission transfer.DirectoryAdmission
	if len(admissions) == 1 {
		admission = admissions[0]
	}
	claim := directoryClaim{
		authority: authority, id: id, parentID: parentID, locator: locator, modified: modified,
		admission: admission,
	}
	if !claim.valid() {
		return directoryClaim{}, ErrInvalidClaim
	}
	return claim, nil
}

func (authority *Authority) bindDirectoryClaim(claim outputsession.DirectoryClaim) (directoryClaim, error) {
	if authority == nil || claim.Admission().IsZero() {
		return directoryClaim{}, ErrInvalidClaim
	}
	directory := claim.Directory()
	locator, err := authority.canonicalLocator(directory.Path)
	if err != nil || locator.canonicalKey != claim.LocatorKey() {
		return directoryClaim{}, errors.Join(ErrInvalidClaim, err)
	}
	native, err := authority.newDirectoryClaim(
		claim.ID(), claim.ParentID(), locator, directory.ModifiedTime, claim.Admission(),
	)
	if err != nil || native.locator.isRoot() != claim.IsRoot() {
		return directoryClaim{}, errors.Join(ErrInvalidClaim, err)
	}
	return native, nil
}

func (authority *Authority) MaterializeDirectory(
	ctx context.Context,
	claim outputsession.DirectoryClaim,
) (outputsession.DirectoryMaterialization, error) {
	if authority == nil {
		err := noMutation(ErrInvalidClaim)
		return outputsession.DirectoryMaterialization{Cut: mutationCut(err)}, directoryBoundaryError(ctx, err)
	}
	native, bindErr := authority.bindDirectoryClaim(claim)
	if bindErr != nil {
		err := noMutation(bindErr)
		authority.emit(TraceEvent{
			Operation: TraceMaterializeDirectory, Outcome: TraceNoMutation,
			ClaimID: claim.ID(), ParentID: claim.ParentID(),
		})
		return outputsession.DirectoryMaterialization{Cut: mutationCut(err)}, directoryBoundaryError(ctx, err)
	}
	authority.gate.RLock()
	result, cached, err := authority.materializeDirectory(ctx, native)
	authority.gate.RUnlock()
	authority.emit(TraceEvent{
		Operation: TraceMaterializeDirectory, Outcome: traceOutcome(err, false), ClaimID: native.id,
		ParentID: native.parentID, Disposition: result.disposition, Cached: cached,
	})
	if err != nil {
		return outputsession.DirectoryMaterialization{Cut: mutationCut(err)}, directoryBoundaryError(ctx, err)
	}
	return outputsession.DirectoryMaterialization{
		Cut: outputsession.MutationStable, Disposition: result.disposition,
	}, nil
}

func (authority *Authority) FinalizeDirectory(
	ctx context.Context,
	claim outputsession.DirectoryClaim,
) (outputsession.DirectoryFinalization, error) {
	if authority == nil {
		err := noMutation(ErrInvalidClaim)
		return outputsession.DirectoryFinalization{Cut: mutationCut(err)}, directoryBoundaryError(ctx, err)
	}
	native, bindErr := authority.bindDirectoryClaim(claim)
	if bindErr != nil {
		err := noMutation(bindErr)
		authority.emit(TraceEvent{
			Operation: TraceFinalizeDirectory, Outcome: TraceNoMutation,
			ClaimID: claim.ID(), ParentID: claim.ParentID(),
		})
		return outputsession.DirectoryFinalization{Cut: mutationCut(err)}, directoryBoundaryError(ctx, err)
	}
	authority.gate.RLock()
	result, disposition, parentID, cached, err := authority.finalizeDirectory(ctx, native)
	authority.gate.RUnlock()
	authority.emit(TraceEvent{
		Operation: TraceFinalizeDirectory,
		Outcome:   traceOutcome(err, result.kind == outputsession.DirectoryFinalizationIsolatedFailure),
		ClaimID:   native.id, ParentID: parentID, Disposition: disposition, Cached: cached,
	})
	if err != nil {
		return outputsession.DirectoryFinalization{Cut: mutationCut(err)}, directoryBoundaryError(ctx, err)
	}
	if result.kind == outputsession.DirectoryFinalizationFinalized {
		return outputsession.FinalizedDirectory(), nil
	}
	observation, observationErr := outputsession.IsolatedDirectory(result.failure)
	if observationErr != nil {
		err = mutationAmbiguous(observationErr)
		return outputsession.DirectoryFinalization{Cut: mutationCut(err)}, directoryBoundaryError(ctx, err)
	}
	return observation, nil
}

func (authority *Authority) Close() error {
	if authority == nil {
		return nil
	}
	authority.gate.Lock()
	authority.mu.Lock()
	if authority.closed {
		authority.mu.Unlock()
		authority.gate.Unlock()
		return nil
	}
	authority.closed = true
	retained := make([]outputcap.Directory, 0, len(authority.claims))
	for _, record := range authority.claims {
		if record.retained != nil {
			retained = append(retained, record.retained)
			record.retained = nil
		}
	}
	authority.claims = nil
	authority.admissions = nil
	authority.mu.Unlock()

	var resultErr error
	for _, directory := range retained {
		resultErr = errors.Join(resultErr, directory.Close())
	}
	authority.gate.Unlock()
	return directoryBoundaryError(context.Background(), resultErr)
}

func (authority *Authority) emit(event TraceEvent) {
	if authority != nil && authority.trace != nil {
		authority.trace(event)
	}
}

func traceOutcome(err error, isolated bool) TraceOutcome {
	switch {
	case errors.Is(err, ErrMutationAmbiguous):
		return TraceMutationAmbiguous
	case errors.Is(err, ErrNoMutation):
		return TraceNoMutation
	case isolated:
		return TraceIsolatedFailure
	default:
		return TraceSucceeded
	}
}

func (authority *Authority) beginMaterialization(
	claim directoryClaim,
) (*claimRecord, bool, directoryMaterialization, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return nil, false, directoryMaterialization{}, noMutation(ErrAuthorityClosed)
	}
	if !claim.valid() || claim.authority != authority {
		return nil, false, directoryMaterialization{}, noMutation(ErrInvalidClaim)
	}
	if !claim.admission.IsZero() {
		admissionKey := string(claim.admission.Bytes())
		if owner, exists := authority.admissions[admissionKey]; exists && owner != claim.id {
			return nil, false, directoryMaterialization{}, noMutation(ErrClaimConflict)
		}
	}
	if existing := authority.claims[claim.id]; existing != nil {
		if !sameDirectoryClaim(existing.claim, claim) {
			return nil, false, directoryMaterialization{}, noMutation(ErrClaimConflict)
		}
		switch existing.state {
		case materializationReady:
			return existing, true, existing.materialization, nil
		case materializationAmbiguous:
			return existing, true, directoryMaterialization{}, mutationAmbiguous(ErrClaimConflict)
		case materializationPending:
			// outputsession owns request coalescing. Seeing a duplicate executor
			// call while mutation is active is therefore a contract breach whose
			// outcome this lower layer must not guess.
			return existing, true, directoryMaterialization{}, mutationAmbiguous(ErrClaimConflict)
		}
	}
	if claim.locator.isRoot() {
		if validClaimID(authority.rootClaimID) && authority.rootClaimID != claim.id {
			return nil, false, directoryMaterialization{}, noMutation(ErrClaimConflict)
		}
		authority.rootClaimID = claim.id
	} else {
		parent := authority.claims[claim.parentID]
		if parent == nil || parent.state != materializationReady || parent.retained == nil {
			return nil, false, directoryMaterialization{}, noMutation(ErrParentUnavailable)
		}
	}
	record := &claimRecord{claim: claim, state: materializationPending}
	authority.claims[claim.id] = record
	return record, false, directoryMaterialization{}, nil
}

func (authority *Authority) finishMaterialization(
	record *claimRecord,
	result directoryMaterialization,
	retained outputcap.Directory,
	err error,
) {
	authority.mu.Lock()
	switch {
	case err == nil && result.valid() && retained != nil:
		record.state = materializationReady
		record.materialization = result
		record.retained = retained
		if !record.claim.admission.IsZero() {
			authority.admissions[string(record.claim.admission.Bytes())] = record.claim.id
		}
	case errors.Is(err, ErrMutationAmbiguous):
		record.state = materializationAmbiguous
		record.retained = retained
	default:
		delete(authority.claims, record.claim.id)
		if !record.claim.admission.IsZero() {
			delete(authority.admissions, string(record.claim.admission.Bytes()))
		}
		if record.claim.locator.isRoot() && authority.rootClaimID == record.claim.id {
			authority.rootClaimID = 0
		}
	}
	authority.mu.Unlock()
}
