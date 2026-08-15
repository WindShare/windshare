package directoryauthority

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type materializeAttempt struct {
	materialization directoryMaterialization
	retained        outputcap.Directory
	mutated         bool
	reconciled      bool
	err             error
}

func (authority *Authority) materializeDirectory(
	ctx context.Context,
	claim directoryClaim,
) (directoryMaterialization, bool, error) {
	if ctx == nil {
		return directoryMaterialization{}, false, noMutation(ErrInvalidClaim)
	}
	record, cached, result, beginErr := authority.beginMaterialization(claim)
	if beginErr != nil || cached {
		return result, cached, beginErr
	}
	if err := ctx.Err(); err != nil {
		err = noMutation(err)
		authority.finishMaterialization(record, directoryMaterialization{}, nil, err)
		return directoryMaterialization{}, false, err
	}
	if err := authority.platform.ValidateModifiedTime(claim.modified); err != nil {
		err = noMutation(err)
		authority.finishMaterialization(record, directoryMaterialization{}, nil, err)
		return directoryMaterialization{}, false, err
	}

	var attempt materializeAttempt
	if claim.locator.isRoot() {
		attempt = authority.materializeRoot(claim)
	} else {
		attempt = authority.materializeChild(ctx, claim)
	}
	if attempt.err == nil {
		attempt.materialization.reconciled = attempt.reconciled
	}
	authority.finishMaterialization(record, attempt.materialization, attempt.retained, attempt.err)
	return attempt.materialization, false, attempt.err
}

func (authority *Authority) materializeRoot(claim directoryClaim) materializeAttempt {
	guard, err := authority.platform.AcquirePublicOperationGuard()
	if err != nil {
		return materializeAttempt{err: noMutation(err)}
	}
	root := guard.Root()
	if root == nil {
		return materializeAttempt{err: noMutation(errors.Join(ErrRetainedAuthorityChanged, guard.Close()))}
	}
	retained, duplicateErr := root.Duplicate()
	if duplicateErr == nil && retained != nil {
		var same bool
		same, duplicateErr = retained.SameDirectory(root)
		if duplicateErr == nil && !same {
			duplicateErr = ErrRetainedAuthorityChanged
		}
	}
	closeErr := guard.Close()
	if duplicateErr != nil || closeErr != nil || retained == nil {
		return materializeAttempt{err: noMutation(errors.Join(
			ErrRetainedAuthorityChanged, duplicateErr, closeErr, closeDirectory(retained),
		))}
	}
	disposition := DirectoryCallerProvidedRoot
	if authority.rootDisposition == outputcap.AuthorityCreatedRoot {
		disposition = DirectoryAuthorityCreatedRoot
	}
	return materializeAttempt{
		materialization: directoryMaterialization{claimID: claim.id, disposition: disposition},
		retained:        retained,
	}
}

func (authority *Authority) materializeChild(
	ctx context.Context,
	claim directoryClaim,
) materializeAttempt {
	snapshot, err := authority.snapshotForChild(claim)
	if err != nil {
		return materializeAttempt{err: noMutation(err)}
	}
	snapshotDecision := snapshot.validateCandidate(claim.locator)
	if err := ctx.Err(); err != nil {
		return materializeAttempt{err: noMutation(err)}
	}
	directory, cleanup, err := authority.openGuardedDirectory(claim.parentID)
	if err != nil {
		return materializeAttempt{err: noMutation(err)}
	}
	attempt := materializeAtParent(directory, claim, snapshotDecision)
	cleanupErr := cleanup()
	if cleanupErr != nil {
		if attempt.mutated {
			attempt.err = mutationAmbiguous(errors.Join(attempt.err, cleanupErr))
		} else {
			attempt.err = noMutation(errors.Join(attempt.err, cleanupErr, closeDirectory(attempt.retained)))
			attempt.retained = nil
		}
	}
	return attempt
}

func (authority *Authority) snapshotForChild(claim directoryClaim) (parentNamespaceIndex, error) {
	if claim.parentID == 0 {
		if !validateImmediateChild("", claim.locator.canonicalPath) {
			return parentNamespaceIndex{}, ErrParentUnavailable
		}
		return authority.executionRootSnapshot()
	}
	authority.mu.Lock()
	parent := authority.claims[claim.parentID]
	authority.mu.Unlock()
	if parent == nil || parent.state != materializationReady ||
		!validateImmediateChild(parent.claim.locator.canonicalPath, claim.locator.canonicalPath) {
		return parentNamespaceIndex{}, ErrParentUnavailable
	}
	return authority.parentSnapshot(parent)
}

func materializeAtParent(
	parent outputcap.Directory,
	claim directoryClaim,
	snapshotDecision error,
) materializeAttempt {
	kind, exact, err := parent.ClassifyExactEntry(claim.locator.leaf)
	if err != nil {
		return materializeAttempt{err: noMutation(err)}
	}
	if kind != outputcap.EntryAbsent {
		if kind == outputcap.EntryDirectory && exact && snapshotDecision == nil {
			retained, openErr := openExactDirectory(parent, claim.locator.leaf)
			if openErr != nil {
				return materializeAttempt{err: noMutation(openErr)}
			}
			return materializeAttempt{
				materialization: directoryMaterialization{
					claimID: claim.id, disposition: DirectoryPreexistingDescendant,
				},
				retained: retained,
			}
		}
		collision := ErrEntryCollision
		if !exact || errors.Is(snapshotDecision, ErrPlatformEquivalentLocator) {
			collision = errors.Join(collision, ErrPlatformEquivalentLocator)
		}
		return materializeAttempt{err: noMutation(errors.Join(collision, snapshotDecision))}
	}
	if validator, ok := parent.(outputcap.CreateAuthorityValidator); ok {
		if err := validator.ValidateCreateAuthority(); err != nil {
			return materializeAttempt{err: noMutation(err)}
		}
	}
	created, createErr := parent.CreateDirectory(claim.locator.leaf, false)
	if createErr != nil {
		return reconcileCreateError(parent, claim, created, createErr)
	}
	if created == nil {
		return materializeAttempt{mutated: true, err: mutationAmbiguous(ErrRetainedAuthorityChanged)}
	}
	if err := revalidateCreatedDirectory(parent, claim.locator.leaf, created); err != nil {
		return materializeAttempt{retained: created, mutated: true, err: mutationAmbiguous(err)}
	}
	if err := errors.Join(created.Sync(), parent.Sync()); err != nil {
		return materializeAttempt{retained: created, mutated: true, err: mutationAmbiguous(err)}
	}
	return materializeAttempt{
		materialization: directoryMaterialization{
			claimID: claim.id, disposition: DirectoryAuthorityCreatedDescendant,
		},
		retained: created, mutated: true,
	}
}

func reconcileCreateError(
	parent outputcap.Directory,
	claim directoryClaim,
	created outputcap.Directory,
	createErr error,
) materializeAttempt {
	if created != nil {
		if err := revalidateCreatedDirectory(parent, claim.locator.leaf, created); err != nil {
			return materializeAttempt{
				retained: created, mutated: true, err: mutationAmbiguous(errors.Join(createErr, err)),
			}
		}
		if err := errors.Join(created.Sync(), parent.Sync()); err != nil {
			return materializeAttempt{
				retained: created, mutated: true, err: mutationAmbiguous(errors.Join(createErr, err)),
			}
		}
		return materializeAttempt{
			materialization: directoryMaterialization{
				claimID: claim.id, disposition: DirectoryAuthorityCreatedDescendant,
			},
			retained: created, mutated: true, reconciled: true,
		}
	}
	kind, exact, observeErr := parent.ClassifyExactEntry(claim.locator.leaf)
	if observeErr == nil && kind == outputcap.EntryAbsent {
		return materializeAttempt{err: noMutation(createErr)}
	}
	// The exclusive-create contract proves a reported collision made no change,
	// even if another actor won the name between the two guarded observations.
	if errors.Is(createErr, outputcap.ErrNamespaceCollision) {
		return materializeAttempt{err: noMutation(errors.Join(createErr, observeErr))}
	}
	return materializeAttempt{
		mutated: true,
		err: mutationAmbiguous(errors.Join(
			createErr, observeErr,
			fmt.Errorf("post-create entry kind=%d exact=%t", kind, exact),
		)),
	}
}

func revalidateCreatedDirectory(
	parent outputcap.Directory,
	name string,
	created outputcap.Directory,
) error {
	reopened, err := openExactDirectory(parent, name)
	if err != nil {
		return err
	}
	same, compareErr := created.SameDirectory(reopened)
	closeErr := reopened.Close()
	if compareErr != nil || closeErr != nil || !same {
		return errors.Join(ErrRetainedAuthorityChanged, compareErr, closeErr)
	}
	return nil
}
