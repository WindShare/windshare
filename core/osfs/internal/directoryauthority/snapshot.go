package directoryauthority

import (
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type capabilityNameSnapshotter struct{}

func (capabilityNameSnapshotter) SnapshotPublicEntryNames(
	directory outputcap.Directory,
	limit int,
) ([]PublicEntryName, error) {
	names, err := directory.Names(limit)
	if err != nil {
		return nil, err
	}
	entries := make([]PublicEntryName, len(names))
	for index, name := range names {
		entries[index] = PublicEntryName{Name: name}
	}
	return entries, nil
}

type parentNamespaceIndex struct {
	owners map[string]string
}

func (authority *Authority) parentSnapshot(record *claimRecord) (parentNamespaceIndex, error) {
	record.snapshotOnce.Do(func() {
		record.snapshot, record.snapshotErr = authority.buildParentSnapshot(record.claim.id)
		if record.snapshotErr != nil {
			record.snapshotErr = errors.Join(ErrParentSnapshotUnavailable, record.snapshotErr)
		}
	})
	return record.snapshot, record.snapshotErr
}

func (authority *Authority) buildParentSnapshot(claimID ClaimID) (parentNamespaceIndex, error) {
	directory, cleanup, err := authority.openGuardedDirectory(claimID)
	if err != nil {
		return parentNamespaceIndex{}, err
	}
	entries, snapshotErr := authority.snapshotter.SnapshotPublicEntryNames(directory, authority.snapshotLimit)
	cleanupErr := cleanup()
	if snapshotErr != nil || cleanupErr != nil {
		return parentNamespaceIndex{}, errors.Join(snapshotErr, cleanupErr)
	}
	if len(entries) > authority.snapshotLimit {
		return parentNamespaceIndex{}, errors.Join(
			ErrParentSnapshotUnavailable,
			fmt.Errorf("snapshot returned %d entries above limit %d", len(entries), authority.snapshotLimit),
		)
	}
	index := parentNamespaceIndex{owners: make(map[string]string, len(entries))}
	for _, entry := range entries {
		if entry.Name == "" || len(entry.Aliases) > MaximumPlatformAliasesPerEntry {
			return parentNamespaceIndex{}, ErrParentSnapshotUnavailable
		}
		if err := authority.indexSnapshotSpelling(index.owners, entry.Name, entry.Name); err != nil {
			return parentNamespaceIndex{}, err
		}
		for _, alias := range entry.Aliases {
			if alias == "" {
				return parentNamespaceIndex{}, ErrParentSnapshotUnavailable
			}
			if err := authority.indexSnapshotSpelling(index.owners, alias, entry.Name); err != nil {
				return parentNamespaceIndex{}, err
			}
		}
	}
	return index, nil
}

func (authority *Authority) indexSnapshotSpelling(
	owners map[string]string,
	spelling string,
	owner string,
) error {
	key, err := authority.platform.CanonicalComponentKey(spelling)
	if err != nil || key == "" {
		return errors.Join(ErrParentSnapshotUnavailable, err)
	}
	if previous, exists := owners[key]; exists && previous != owner {
		return errors.Join(ErrParentSnapshotUnavailable, ErrPlatformEquivalentLocator)
	}
	owners[key] = owner
	return nil
}

func (index parentNamespaceIndex) validateCandidate(locator locatorKey) error {
	if owner, exists := index.owners[locator.leafKey]; exists && owner != locator.leaf {
		return errors.Join(ErrPlatformEquivalentLocator, ErrEntryCollision)
	}
	return nil
}
