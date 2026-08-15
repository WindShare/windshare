package checkpointstore

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

const maximumOrdinaryLockFilesV1 = MaximumOrdinaryOperationRecordsV1 * 3

// RecyclePrivateState removes empty current-generation infrastructure after the
// destination lifecycle protocol proves that no bound process can still use it.
// Recovery evidence and every unknown or non-empty shape are preserved.
func RecyclePrivateState(control outputcap.Directory) (bool, error) {
	if control == nil {
		return false, nil
	}
	ordinaryEmpty, err := recycleOrdinaryRegistry(control)
	if err != nil {
		return false, fmt.Errorf("recycle ordinary private state: %w", err)
	}
	if !ordinaryEmpty {
		return false, nil
	}
	cleanupEmpty, err := recycleLiveCleanupJournal(control)
	if err != nil {
		return false, fmt.Errorf("recycle live cleanup state: %w", err)
	}
	if !cleanupEmpty {
		return false, nil
	}
	return true, nil
}

func recycleLiveCleanupJournal(control outputcap.Directory) (empty bool, resultErr error) {
	proof, found, safe, err := openRecycleDirectory(control, checkpointmodel.LiveCleanupNamespaceV1)
	if err != nil || !safe || !found {
		return safe, err
	}
	proofOwned := true
	defer func() {
		if proofOwned {
			resultErr = errors.Join(resultErr, proof.Close())
		}
	}()
	names, err := proof.Names(2)
	if errors.Is(err, outputcap.ErrUnsafeNamespace) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(names) != 1 || names[0] != liveCleanupTicketsDirectory {
		return false, nil
	}
	tickets, found, safe, err := openRecycleDirectory(proof, liveCleanupTicketsDirectory)
	if err != nil || !safe || !found {
		return false, err
	}
	if empty, err := recycleDirectoryIsEmpty(tickets); err != nil || !empty {
		return false, errors.Join(err, tickets.Close())
	}
	if err := removeEmptyDirectory(proof, liveCleanupTicketsDirectory, tickets); err != nil {
		return false, err
	}
	proofOwned = false
	if err := removeEmptyDirectory(control, checkpointmodel.LiveCleanupNamespaceV1, proof); err != nil {
		return false, err
	}
	return true, nil
}

func recycleOrdinaryRegistry(control outputcap.Directory) (empty bool, resultErr error) {
	root, found, safe, err := openRecycleDirectory(control, OrdinaryRegistryDirectoryV1)
	if err != nil || !safe || !found {
		return safe, err
	}
	rootOwned := true
	defer func() {
		if rootOwned {
			resultErr = errors.Join(resultErr, root.Close())
		}
	}()
	if safe, err := authenticateRecycleEntries(root, ordinaryRegistryEntries); err != nil || !safe {
		return false, err
	}
	for _, spec := range []recycleChildSpec{
		{name: ordinaryOperationsDirectory, childNameHexBytes: 16},
		{name: ordinaryActiveDirectory, childNameHexBytes: 32},
		{name: ordinaryCandidatesDirectory, childNameHexBytes: 32},
	} {
		empty, err := recycleEmptyDirectoryChildren(root, spec)
		if err != nil || !empty {
			return false, err
		}
	}
	if empty, err := recycleRequiredEmptyDirectory(root, ordinaryClaimsDirectory); err != nil || !empty {
		return false, err
	}
	if empty, err := recycleLeaseDirectory(root); err != nil || !empty {
		return false, err
	}
	rootOwned = false
	if err := removeEmptyDirectory(control, OrdinaryRegistryDirectoryV1, root); err != nil {
		return false, err
	}
	return true, nil
}

type recycleChildSpec struct {
	name              string
	childNameHexBytes int
}

func recycleEmptyDirectoryChildren(
	root outputcap.Directory,
	spec recycleChildSpec,
) (empty bool, resultErr error) {
	directory, found, safe, err := openRecycleDirectory(root, spec.name)
	if err != nil || !safe || !found {
		return safe, err
	}
	directoryOwned := true
	defer func() {
		if directoryOwned {
			resultErr = errors.Join(resultErr, directory.Close())
		}
	}()
	names, err := directory.Names(MaximumOrdinaryOperationRecordsV1 + 1)
	if errors.Is(err, outputcap.ErrUnsafeNamespace) {
		return false, nil
	}
	if err != nil || len(names) > MaximumOrdinaryOperationRecordsV1 {
		return false, err
	}
	children := make([]outputcap.Directory, 0, len(names))
	for _, name := range names {
		if !validLowerHexName(name, spec.childNameHexBytes) {
			return false, closeDirectories(children)
		}
		child, found, childSafe, openErr := openRecycleDirectory(directory, name)
		if openErr != nil || !childSafe || !found {
			return false, errors.Join(openErr, closeDirectories(children))
		}
		empty, emptyErr := recycleDirectoryIsEmpty(child)
		if emptyErr != nil || !empty {
			return false, errors.Join(emptyErr, closeDirectories(append(children, child)))
		}
		children = append(children, child)
	}
	for index, name := range names {
		if err := removeEmptyDirectory(directory, name, children[index]); err != nil {
			return false, errors.Join(err, closeDirectories(children[index+1:]))
		}
	}
	directoryOwned = false
	if err := removeEmptyDirectory(root, spec.name, directory); err != nil {
		return false, err
	}
	return true, nil
}

func recycleRequiredEmptyDirectory(root outputcap.Directory, name string) (bool, error) {
	directory, found, safe, err := openRecycleDirectory(root, name)
	if err != nil || !safe || !found {
		return safe, err
	}
	empty, err := recycleDirectoryIsEmpty(directory)
	if err != nil || !empty {
		return false, errors.Join(err, directory.Close())
	}
	if err := removeEmptyDirectory(root, name, directory); err != nil {
		return false, err
	}
	return true, nil
}

func recycleLeaseDirectory(root outputcap.Directory) (empty bool, resultErr error) {
	directory, found, safe, err := openRecycleDirectory(root, ordinaryLeasesDirectory)
	if err != nil || !safe || !found {
		return safe, err
	}
	directoryOwned := true
	defer func() {
		if directoryOwned {
			resultErr = errors.Join(resultErr, directory.Close())
		}
	}()
	names, err := directory.Names(maximumOrdinaryLockFilesV1 + 1)
	if errors.Is(err, outputcap.ErrUnsafeNamespace) {
		return false, nil
	}
	if err != nil || len(names) > maximumOrdinaryLockFilesV1 {
		return false, err
	}
	for _, name := range names {
		if !validOrdinaryLockName(name) {
			return false, nil
		}
		kind, exact, classifyErr := directory.ClassifyExactEntry(name)
		if classifyErr != nil || !exact || kind != outputcap.EntryRegularFile {
			return false, classifyErr
		}
	}
	for _, name := range names {
		lock, _, lockErr := directory.AcquireLock(name, true)
		if errors.Is(lockErr, outputcap.ErrNamespaceLockBusy) {
			return false, nil
		}
		if lockErr != nil || lock == nil {
			return false, errors.Join(lockErr, closeLock(lock))
		}
		removeErr := directory.RemoveFile(name, lock.File())
		closeErr := lock.Close()
		if removeErr != nil || closeErr != nil {
			return false, errors.Join(removeErr, closeErr)
		}
	}
	if len(names) != 0 {
		if err := directory.Sync(); err != nil {
			return false, err
		}
	}
	directoryOwned = false
	if err := removeEmptyDirectory(root, ordinaryLeasesDirectory, directory); err != nil {
		return false, err
	}
	return true, nil
}

func openRecycleDirectory(
	parent outputcap.Directory,
	name string,
) (outputcap.Directory, bool, bool, error) {
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return nil, false, false, err
	}
	if kind == outputcap.EntryAbsent {
		return nil, false, true, nil
	}
	if !exact || kind != outputcap.EntryDirectory {
		return nil, true, false, nil
	}
	directory, err := openExistingDirectory(parent, name)
	if err != nil {
		return nil, true, false, err
	}
	return directory, true, true, nil
}

func authenticateRecycleEntries(
	directory outputcap.Directory,
	allowed map[string]outputcap.EntryKind,
) (bool, error) {
	names, err := directory.Names(len(allowed) + 1)
	if errors.Is(err, outputcap.ErrUnsafeNamespace) {
		return false, nil
	}
	if err != nil || len(names) > len(allowed) {
		return false, err
	}
	for _, name := range names {
		expected, known := allowed[name]
		if !known {
			return false, nil
		}
		kind, exact, classifyErr := directory.ClassifyExactEntry(name)
		if classifyErr != nil || !exact || kind != expected {
			return false, classifyErr
		}
	}
	return true, nil
}

func recycleDirectoryIsEmpty(directory outputcap.Directory) (bool, error) {
	names, err := directory.Names(1)
	if errors.Is(err, outputcap.ErrUnsafeNamespace) {
		return false, nil
	}
	return err == nil && len(names) == 0, err
}

func validOrdinaryLockName(name string) bool {
	for _, spec := range []struct {
		suffix   string
		hexBytes int
	}{
		{suffix: ordinaryOperationLockSuffix, hexBytes: 16},
		{suffix: ordinaryActiveLockSuffix, hexBytes: 32},
		{suffix: ordinaryClaimLockSuffix, hexBytes: 32},
	} {
		prefix, found := strings.CutSuffix(name, spec.suffix)
		if found {
			return validLowerHexName(prefix, spec.hexBytes)
		}
	}
	return false
}

func validLowerHexName(name string, bytes int) bool {
	if len(name) != bytes*2 || name != strings.ToLower(name) {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

func closeDirectories(directories []outputcap.Directory) error {
	var resultErr error
	for _, directory := range directories {
		resultErr = errors.Join(resultErr, closeDirectory(directory))
	}
	return resultErr
}
