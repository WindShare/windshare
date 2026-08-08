package directoryauthority

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
)

type directoryMetadataMatcher interface {
	MetadataMatches(catalog.ModifiedTime) (bool, error)
}

func (authority *Authority) finalizeDirectory(
	ctx context.Context,
	claim directoryClaim,
) (directoryFinalization, DirectoryDisposition, ClaimID, bool, error) {
	if ctx == nil {
		return directoryFinalization{}, 0, claim.parentID, false, noMutation(ErrInvalidClaim)
	}
	record, cached, result, err := authority.beginFinalization(claim)
	if err != nil || cached {
		disposition, parentID := recordContext(record)
		return result, disposition, parentID, cached, err
	}
	if err := ctx.Err(); err != nil {
		err = noMutation(err)
		authority.finishFinalization(record, directoryFinalization{}, err)
		return directoryFinalization{}, record.materialization.disposition, record.claim.parentID, false, err
	}
	result, finalizeErr := authority.finalizeRetained(record)
	authority.finishFinalization(record, result, finalizeErr)
	return result, record.materialization.disposition, record.claim.parentID, false, finalizeErr
}

func (authority *Authority) beginFinalization(
	claim directoryClaim,
) (*claimRecord, bool, directoryFinalization, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return nil, false, directoryFinalization{}, noMutation(ErrAuthorityClosed)
	}
	record := authority.claims[claim.id]
	if record == nil || record.state != materializationReady || record.retained == nil {
		return record, false, directoryFinalization{}, noMutation(ErrParentUnavailable)
	}
	if !sameDirectoryClaim(record.claim, claim) {
		return record, false, directoryFinalization{}, noMutation(ErrClaimConflict)
	}
	switch record.finalizationState {
	case finalizationSettled:
		return record, true, record.finalization, nil
	case finalizationAmbiguous:
		return record, true, directoryFinalization{}, mutationAmbiguous(ErrMetadataReconcile)
	case finalizationPending:
		// Coalescing is outputsession state, not native filesystem authority.
		return record, true, directoryFinalization{}, mutationAmbiguous(ErrMetadataReconcile)
	default:
		record.finalizationState = finalizationPending
		return record, false, directoryFinalization{}, nil
	}
}

func recordContext(record *claimRecord) (DirectoryDisposition, ClaimID) {
	if record == nil {
		return 0, 0
	}
	return record.materialization.disposition, record.claim.parentID
}

func (authority *Authority) finishFinalization(
	record *claimRecord,
	result directoryFinalization,
	err error,
) {
	authority.mu.Lock()
	switch {
	case err == nil && result.valid():
		record.finalizationState = finalizationSettled
		record.finalization = result
	case errors.Is(err, ErrNoMutation):
		record.finalizationState = finalizationUnstarted
	default:
		record.finalizationState = finalizationAmbiguous
	}
	authority.mu.Unlock()
}

func (authority *Authority) finalizeRetained(record *claimRecord) (directoryFinalization, error) {
	directory, cleanup, err := authority.openGuardedDirectory(record.claim.id)
	if err != nil {
		return directoryFinalization{}, mutationAmbiguous(err)
	}
	result, mutationStarted, finalizeErr := finalizeDirectoryMetadata(directory, record)
	cleanupErr := cleanup()
	if cleanupErr != nil {
		if mutationStarted {
			return directoryFinalization{}, mutationAmbiguous(errors.Join(finalizeErr, cleanupErr))
		}
		return directoryFinalization{}, mutationAmbiguous(errors.Join(ErrRetainedAuthorityChanged, finalizeErr, cleanupErr))
	}
	return result, finalizeErr
}

func finalizeDirectoryMetadata(
	directory outputcap.Directory,
	record *claimRecord,
) (directoryFinalization, bool, error) {
	// A retained path proves confinement and exact identity for this run, but only
	// backend creation proves ownership strongly enough to mutate directory metadata.
	modified := record.claim.modified
	if directoryMetadataOwned(record.materialization.disposition) && modified.Present() {
		return finalizeOwnedDirectoryMetadata(directory, record.claim.id, modified)
	}
	return finalizeDirectoryWithoutMetadata(directory, record.claim.id)
}

func finalizeOwnedDirectoryMetadata(
	directory outputcap.Directory,
	claimID ClaimID,
	modified catalog.ModifiedTime,
) (directoryFinalization, bool, error) {
	if validator, ok := directory.(outputcap.MetadataAuthorityValidator); ok {
		if err := validator.ValidateMetadataAuthority(); err != nil {
			if syncErr := directory.Sync(); syncErr != nil {
				return directoryFinalization{}, false, mutationAmbiguous(errors.Join(err, syncErr))
			}
			return isolatedMetadataFinalization(claimID), false, nil
		}
	}
	if err := directory.SetModifiedTime(modified); err != nil {
		return reconcileMetadataError(directory, claimID, modified, err)
	}
	if err := directory.Sync(); err != nil {
		return directoryFinalization{}, true, mutationAmbiguous(err)
	}
	return directoryFinalization{
		claimID: claimID, kind: outputsession.DirectoryFinalizationFinalized,
	}, true, nil
}

func finalizeDirectoryWithoutMetadata(
	directory outputcap.Directory,
	claimID ClaimID,
) (directoryFinalization, bool, error) {
	if err := directory.Sync(); err != nil {
		return directoryFinalization{}, false, mutationAmbiguous(err)
	}
	return directoryFinalization{
		claimID: claimID, kind: outputsession.DirectoryFinalizationFinalized,
	}, false, nil
}

func reconcileMetadataError(
	directory outputcap.Directory,
	claimID ClaimID,
	modified catalog.ModifiedTime,
	setErr error,
) (directoryFinalization, bool, error) {
	matcher, ok := directory.(directoryMetadataMatcher)
	if !ok {
		return directoryFinalization{}, true, mutationAmbiguous(errors.Join(ErrMetadataReconcile, setErr))
	}
	matches, observeErr := matcher.MetadataMatches(modified)
	if observeErr != nil {
		return directoryFinalization{}, true, mutationAmbiguous(errors.Join(ErrMetadataReconcile, setErr, observeErr))
	}
	if syncErr := directory.Sync(); syncErr != nil {
		return directoryFinalization{}, true, mutationAmbiguous(errors.Join(setErr, syncErr))
	}
	if matches {
		return directoryFinalization{
			claimID: claimID, kind: outputsession.DirectoryFinalizationFinalized, reconciled: true,
		}, true, nil
	}
	return isolatedMetadataFinalization(claimID), true, nil
}

func isolatedMetadataFinalization(claimID ClaimID) directoryFinalization {
	return directoryFinalization{
		claimID: claimID, kind: outputsession.DirectoryFinalizationIsolatedFailure,
		failure: metadataFault(), reconciled: true,
	}
}
