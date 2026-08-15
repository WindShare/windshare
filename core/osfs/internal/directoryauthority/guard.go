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
	if len(reversed) == 0 {
		return nil, ErrRetainedAuthorityChanged
	}
	top := reversed[len(reversed)-1].claim
	if top.parentID != 0 || !top.locator.isRoot() && !validateImmediateChild("", top.locator.canonicalPath) {
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
	if claimID == 0 {
		guard, root, err := acquireGuardedRoot(authority.platform)
		if err != nil {
			return nil, nil, err
		}
		return root, guard.Close, nil
	}
	lineage, err := authority.readyLineage(claimID)
	if err != nil {
		return nil, nil, err
	}
	guard, root, err := acquireGuardedRoot(authority.platform)
	if err != nil {
		return nil, nil, err
	}
	current, currentOwned, err := walkRetainedLineage(root, lineage)
	if err != nil {
		return nil, nil, errors.Join(err, guard.Close())
	}
	return current, guardedDirectoryCleanup(current, currentOwned, guard), nil
}

func acquireGuardedRoot(
	platform Platform,
) (outputcap.PublicOperationGuard, outputcap.Directory, error) {
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, nil, err
	}
	root := guard.Root()
	if root == nil {
		return nil, nil, errors.Join(ErrRetainedAuthorityChanged, guard.Close())
	}
	return guard, root, nil
}

func walkRetainedLineage(
	root outputcap.Directory,
	lineage []*claimRecord,
) (outputcap.Directory, bool, error) {
	current := root
	currentOwned := false
	start := 0
	if lineage[0].claim.locator.isRoot() {
		same, err := lineage[0].retained.SameDirectory(root)
		if err != nil || !same {
			return nil, false, errors.Join(ErrRetainedAuthorityChanged, err)
		}
		start = 1
	}
	for _, record := range lineage[start:] {
		next, err := openExactDirectory(current, record.claim.locator.leaf)
		if err == nil {
			var same bool
			same, err = record.retained.SameDirectory(next)
			if err == nil && !same {
				err = ErrRetainedAuthorityChanged
			}
		}
		if err != nil {
			return nil, false, errors.Join(
				ErrRetainedAuthorityChanged, err, closeDirectory(next), closeOwnedDirectory(current, currentOwned),
			)
		}
		if err := closeOwnedDirectory(current, currentOwned); err != nil {
			return nil, false, errors.Join(ErrRetainedAuthorityChanged, err, next.Close())
		}
		current, currentOwned = next, true
	}
	return current, currentOwned, nil
}

func guardedDirectoryCleanup(
	current outputcap.Directory,
	currentOwned bool,
	guard outputcap.PublicOperationGuard,
) func() error {
	closed := false
	return func() error {
		if closed {
			return nil
		}
		closed = true
		return errors.Join(closeOwnedDirectory(current, currentOwned), guard.Close())
	}
}

func closeOwnedDirectory(directory outputcap.Directory, owned bool) error {
	if !owned || directory == nil {
		return nil
	}
	return directory.Close()
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
