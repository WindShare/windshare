package directoryauthority

import (
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func (authority *Authority) readyLineage(claimID ClaimID) ([]*claimRecord, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return nil, ErrAuthorityClosed
	}
	reversed := make([]*claimRecord, 0, min(catalog.MaxPathDepth+1, len(authority.claims)))
	seen := make(map[ClaimID]struct{}, catalog.MaxPathDepth+1)
	for currentID := claimID; validClaimID(currentID); {
		if _, duplicate := seen[currentID]; duplicate || len(reversed) > catalog.MaxPathDepth {
			return nil, ErrRetainedAuthorityChanged
		}
		seen[currentID] = struct{}{}
		record := authority.claims[currentID]
		if record == nil || record.state != materializationReady || record.retained == nil {
			return nil, ErrParentUnavailable
		}
		reversed = append(reversed, record)
		currentID = record.claim.parentID
	}
	if len(reversed) == 0 || !reversed[len(reversed)-1].claim.locator.isRoot() {
		return nil, ErrRetainedAuthorityChanged
	}
	lineage := make([]*claimRecord, len(reversed))
	for index := range reversed {
		lineage[len(reversed)-1-index] = reversed[index]
	}
	return lineage, nil
}

// openGuardedDirectory reacquires placement authority, walks only the retained
// claim's ancestry, and proves each lexical edge against its live handle. The
// returned capability is borrowed until cleanup.
func (authority *Authority) openGuardedDirectory(
	claimID ClaimID,
) (outputcap.Directory, func() error, error) {
	lineage, err := authority.readyLineage(claimID)
	if err != nil {
		return nil, nil, err
	}
	guard, err := authority.platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, nil, err
	}
	root := guard.Root()
	if root == nil {
		return nil, nil, errors.Join(ErrRetainedAuthorityChanged, guard.Close())
	}
	same, err := lineage[0].retained.SameDirectory(root)
	if err != nil || !same {
		return nil, nil, errors.Join(ErrRetainedAuthorityChanged, err, guard.Close())
	}
	current := root
	currentOwned := false
	for _, record := range lineage[1:] {
		next, openErr := openExactDirectory(current, record.claim.locator.leaf)
		if openErr == nil {
			same, openErr = record.retained.SameDirectory(next)
			if openErr == nil && !same {
				openErr = ErrRetainedAuthorityChanged
			}
		}
		if openErr != nil {
			if next != nil {
				openErr = errors.Join(openErr, next.Close())
			}
			if currentOwned {
				openErr = errors.Join(openErr, current.Close())
			}
			return nil, nil, errors.Join(ErrRetainedAuthorityChanged, openErr, guard.Close())
		}
		if currentOwned {
			if closeErr := current.Close(); closeErr != nil {
				return nil, nil, errors.Join(
					ErrRetainedAuthorityChanged, closeErr, next.Close(), guard.Close(),
				)
			}
		}
		current, currentOwned = next, true
	}
	closed := false
	cleanup := func() error {
		if closed {
			return nil
		}
		closed = true
		var closeErr error
		if currentOwned {
			closeErr = current.Close()
		}
		return errors.Join(closeErr, guard.Close())
	}
	return current, cleanup, nil
}

func openExactDirectory(parent outputcap.Directory, name string) (outputcap.Directory, error) {
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return nil, err
	}
	if !exact || kind != outputcap.EntryDirectory {
		return nil, fmt.Errorf("%w: retained component has kind %d and exact=%t", ErrRetainedAuthorityChanged, kind, exact)
	}
	reference, err := parent.OpenEntry(name)
	if err != nil {
		return nil, err
	}
	if reference == nil || reference.Kind() != outputcap.EntryDirectory {
		return nil, errors.Join(ErrRetainedAuthorityChanged, closeEntry(reference))
	}
	opened, openErr := parent.OpenPinnedDirectory(reference, false)
	closeErr := reference.Close()
	if openErr != nil || closeErr != nil {
		return nil, errors.Join(openErr, closeErr, closeDirectory(opened))
	}
	return opened, nil
}

func closeEntry(reference outputcap.CurrentEntryReference) error {
	if reference == nil {
		return nil
	}
	return reference.Close()
}

func closeDirectory(directory outputcap.Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}
