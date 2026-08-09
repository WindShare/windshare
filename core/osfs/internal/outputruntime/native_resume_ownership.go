package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func (lease *NativeResumeLease) observeDirectoriesLocked(
	ctx context.Context,
	directories []checkpointmodel.AdmittedDirectory,
) (bool, error) {
	for _, record := range directories {
		pin, err := pinNativeResumeDirectory(ctx, lease.platform, record.CanonicalPath())
		if err != nil {
			return false, err
		}
		if pin.absent {
			stable, revalidateErr := pin.Revalidate()
			closeErr := pin.Close()
			if revalidateErr != nil || closeErr != nil {
				return false, errors.Join(revalidateErr, closeErr)
			}
			if !stable {
				return false, nil
			}
			continue
		}
		owned, identityErr := directoryauthority.PersistentOwnedDirectoryID(pin.directory)
		stable, revalidateErr := pin.Revalidate()
		closeErr := pin.Close()
		if identityErr != nil || revalidateErr != nil || closeErr != nil {
			return false, errors.Join(identityErr, revalidateErr, closeErr)
		}
		if !stable || owned != record.OwnedObjectID() {
			return false, nil
		}
	}
	return true, nil
}

func (lease *NativeResumeLease) cleanupDirectoriesLocked(
	ctx context.Context,
	directories []checkpointmodel.AdmittedDirectory,
) ([]checkpointmodel.ObjectID, error) {
	ordered := slices.Clone(directories)
	slices.Reverse(ordered)
	removed := make([]checkpointmodel.ObjectID, 0, len(ordered))
	for _, record := range ordered {
		if record.CanonicalPath() == "" {
			continue
		}
		pin, err := pinNativeResumeDirectory(ctx, lease.platform, record.CanonicalPath())
		if err != nil {
			return nil, err
		}
		object, objectErr := checkpointmodel.ObjectIDFromBytes(record.OwnedObjectID().Bytes())
		if objectErr != nil {
			return nil, errors.Join(objectErr, pin.Close())
		}
		if pin.absent {
			stable, revalidateErr := pin.Revalidate()
			closeErr := pin.Close()
			if revalidateErr != nil || closeErr != nil || !stable {
				return nil, errors.Join(
					revalidateErr, closeErr, ErrNativeResumeOwnershipUnknown,
				)
			}
			removed = append(removed, object)
			continue
		}
		owned, identityErr := directoryauthority.PersistentOwnedDirectoryID(pin.directory)
		if identityErr != nil || owned != record.OwnedObjectID() {
			return nil, errors.Join(identityErr, ErrNativeResumeOwnershipUnknown, pin.Close())
		}
		names, namesErr := pin.directory.Names(1)
		stable, revalidateErr := pin.Revalidate()
		if namesErr != nil || revalidateErr != nil || !stable {
			return nil, errors.Join(
				namesErr, revalidateErr, ErrNativeResumeOwnershipUnknown, pin.Close(),
			)
		}
		if len(names) != 0 {
			// A retained finalized entry or a caller-created entry makes the
			// directory ineligible for removal, not ownership-uncertain. Discard
			// cleans only WindShare-owned unfinished objects and preserves the
			// non-empty directory without inspecting or mutating its children.
			if closeErr := pin.Close(); closeErr != nil {
				return nil, errors.Join(closeErr, ErrNativeResumeOwnershipUnknown)
			}
			continue
		}
		// Public child handles intentionally deny delete sharing on Windows. Once
		// ownership and emptiness are proven, retain the entry witness and lineage
		// but release the child capability before unlinking through that witness.
		targetCloseErr := pin.directory.Close()
		pin.directory = nil
		if targetCloseErr != nil {
			return nil, errors.Join(targetCloseErr, ErrNativeResumeOwnershipUnknown, pin.Close())
		}
		removeErr := pin.parent.RemoveEntry(pin.leaf, pin.entry)
		kind, exact, classifyErr := pin.parent.ClassifyExactEntry(pin.leaf)
		lineageStable, lineageErr := pin.RevalidateLineage()
		closeErr := pin.Close()
		if removeErr != nil || classifyErr != nil || lineageErr != nil || !lineageStable ||
			!exact || kind != outputcap.EntryAbsent || closeErr != nil {
			return nil, errors.Join(
				removeErr, classifyErr, lineageErr, closeErr, ErrNativeResumeOwnershipUnknown,
			)
		}
		removed = append(removed, object)
	}
	slices.SortFunc(removed, func(left, right checkpointmodel.ObjectID) int {
		return bytes.Compare(left.Bytes(), right.Bytes())
	})
	return removed, nil
}

type nativeResumeDirectoryPin struct {
	guard outputcap.PublicOperationGuard

	directory outputcap.Directory
	parent    outputcap.Directory
	leaf      string
	entry     outputcap.CurrentEntryReference
	lineage   []nativeResumeLineagePin
	opened    []outputcap.Directory

	absent       bool
	absentParent outputcap.Directory
	absentName   string
	closed       bool
}

func pinNativeResumeDirectory(
	ctx context.Context,
	platform outputcap.Platform,
	path string,
) (result *nativeResumeDirectoryPin, resultErr error) {
	components, err := validatedNativeResumeDirectoryComponents(ctx, platform, path)
	if err != nil {
		return nil, err
	}
	pin, err := acquireNativeResumeDirectoryPin(platform)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, pin.Close())
		}
	}()
	if len(components) == 0 {
		pin.directory = pin.guard.Root()
		return pin, nil
	}
	if err := pin.resolveDirectoryPath(components); err != nil {
		return nil, err
	}
	return pin, nil
}

func validatedNativeResumeDirectoryComponents(
	ctx context.Context,
	platform outputcap.Platform,
	path string,
) ([]string, error) {
	if ctx == nil || platform == nil || platform.Root() == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, nil
	}
	canonical, err := catalog.CanonicalPath(path)
	if err != nil || canonical != path {
		return nil, errors.Join(err, checkpointmodel.ErrInvalidAdmittedDirectory)
	}
	if key, err := platform.CanonicalLocatorKey(path); err != nil || key == "" {
		return nil, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	return strings.Split(path, "/"), nil
}

func acquireNativeResumeDirectoryPin(
	platform outputcap.Platform,
) (*nativeResumeDirectoryPin, error) {
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, nativeResumeError(err)
	}
	pin := &nativeResumeDirectoryPin{guard: guard}
	if guard == nil || guard.Root() == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, pin.Close())
	}
	sameRoot, err := platform.Root().SameDirectory(guard.Root())
	if err != nil || !sameRoot {
		return nil, errors.Join(err, outputcap.ErrUnsafeNamespace, pin.Close())
	}
	return pin, nil
}

func (pin *nativeResumeDirectoryPin) resolveDirectoryPath(components []string) error {
	current := pin.guard.Root()
	for index, component := range components {
		kind, exact, classifyErr := current.ClassifyExactEntry(component)
		if classifyErr != nil {
			return classifyErr
		}
		if kind == outputcap.EntryAbsent && exact {
			pin.absent = true
			pin.absentParent = current
			pin.absentName = component
			return nil
		}
		if !exact || kind != outputcap.EntryDirectory {
			return ErrNativeResumeOwnershipUnknown
		}
		entry, err := current.OpenEntry(component)
		if err != nil || entry == nil || entry.Kind() != outputcap.EntryDirectory {
			return errors.Join(err, outputcap.ErrUnsafeNamespace, closeNativeResumeEntry(entry))
		}
		child, err := current.OpenPinnedDirectory(entry, false)
		if err != nil || child == nil {
			return errors.Join(
				err, outputcap.ErrUnsafeNamespace,
				closeNativeResumeEntry(entry), closeNativeResumeDirectory(child),
			)
		}
		if index == len(components)-1 {
			pin.parent = current
			pin.leaf = component
			pin.entry = entry
			pin.directory = child
			pin.opened = append(pin.opened, child)
			return nil
		}
		pin.lineage = append(pin.lineage, nativeResumeLineagePin{
			parent: current, name: component, entry: entry,
		})
		pin.opened = append(pin.opened, child)
		current = child
	}
	return outputcap.ErrUnsafeNamespace
}

func (pin *nativeResumeDirectoryPin) Revalidate() (bool, error) {
	if pin == nil || pin.closed || pin.guard == nil {
		return false, transfer.ErrInvalidOutputBinding
	}
	if pin.absent {
		kind, exact, err := pin.absentParent.ClassifyExactEntry(pin.absentName)
		if err != nil || !exact || kind != outputcap.EntryAbsent {
			return false, err
		}
	} else if pin.entry != nil {
		unchanged, err := pin.parent.EntryMatches(pin.leaf, pin.entry)
		if err != nil || !unchanged {
			return false, err
		}
	}
	return pin.RevalidateLineage()
}

func (pin *nativeResumeDirectoryPin) RevalidateLineage() (bool, error) {
	if pin == nil || pin.closed || pin.guard == nil {
		return false, transfer.ErrInvalidOutputBinding
	}
	for _, lineage := range slices.Backward(pin.lineage) {
		unchanged, err := lineage.parent.EntryMatches(lineage.name, lineage.entry)
		if err != nil || !unchanged {
			return false, err
		}
	}
	return true, nil
}

func (pin *nativeResumeDirectoryPin) Close() error {
	if pin == nil || pin.closed {
		return nil
	}
	pin.closed = true
	var result error
	result = errors.Join(result, closeNativeResumeEntry(pin.entry))
	for _, lineage := range slices.Backward(pin.lineage) {
		result = errors.Join(result, closeNativeResumeEntry(lineage.entry))
	}
	for _, directory := range slices.Backward(pin.opened) {
		result = errors.Join(result, closeNativeResumeDirectory(directory))
	}
	result = errors.Join(result, closeNativeResumeGuard(pin.guard))
	return result
}

type nativeResumeLineagePin struct {
	parent outputcap.Directory
	name   string
	entry  outputcap.CurrentEntryReference
}

func observeNativeResumePublication(
	ctx context.Context,
	platform outputcap.Platform,
	store *checkpointstore.FileExecutionStore,
	record checkpointmodel.Record,
) (proven bool, resultErr error) {
	location, err := validateNativeResumePublication(ctx, platform, store, record)
	if err != nil {
		return false, err
	}
	parent, err := pinNativeResumeDirectory(ctx, platform, location.parentPath)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	if parent.absent {
		return false, nil
	}
	return observeNativeResumePublishedFile(ctx, store, record, parent, location.leaf)
}

type nativeResumePublicationLocation struct {
	parentPath string
	leaf       string
}

func validateNativeResumePublication(
	ctx context.Context,
	platform outputcap.Platform,
	store *checkpointstore.FileExecutionStore,
	record checkpointmodel.Record,
) (nativeResumePublicationLocation, error) {
	if ctx == nil || platform == nil || platform.Root() == nil || store == nil || !record.Valid() {
		return nativeResumePublicationLocation{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nativeResumePublicationLocation{}, err
	}
	canonical, err := catalog.CanonicalPath(record.CanonicalPath())
	if err != nil || canonical != record.CanonicalPath() || canonical == "" {
		return nativeResumePublicationLocation{}, errors.Join(err, checkpointmodel.ErrRecordBinding)
	}
	if key, err := platform.CanonicalLocatorKey(canonical); err != nil || key == "" {
		return nativeResumePublicationLocation{}, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	components := strings.Split(canonical, "/")
	return nativeResumePublicationLocation{
		parentPath: strings.Join(components[:len(components)-1], "/"),
		leaf:       components[len(components)-1],
	}, nil
}

func observeNativeResumePublishedFile(
	ctx context.Context,
	store *checkpointstore.FileExecutionStore,
	record checkpointmodel.Record,
	parent *nativeResumeDirectoryPin,
	leaf string,
) (proven bool, resultErr error) {
	current := parent.directory
	kind, exact, err := current.ClassifyExactEntry(leaf)
	if err != nil {
		return false, err
	}
	if !exact || kind != outputcap.EntryRegularFile {
		return false, nil
	}
	entry, err := current.OpenEntry(leaf)
	if err != nil || entry == nil || entry.Kind() != outputcap.EntryRegularFile {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace, closeNativeResumeEntry(entry))
	}
	defer func() { resultErr = errors.Join(resultErr, closeNativeResumeEntry(entry)) }()
	final, err := current.OpenFile(leaf, false, false)
	if err != nil || final == nil {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace, closeNativeResumeFile(final))
	}
	defer func() { resultErr = errors.Join(resultErr, closeNativeResumeFile(final)) }()
	unchanged, err := current.EntryMatches(leaf, entry)
	if err != nil || !unchanged {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	matches, err := store.FinalMatchesOwned(ctx, record.OwnedObjectID(), record.ExactSize(), final)
	if err != nil || !matches {
		return false, err
	}
	unchanged, err = current.EntryMatches(leaf, entry)
	if err != nil || !unchanged {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	lineageStable, err := parent.Revalidate()
	if err != nil || !lineageStable {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	return true, nil
}

func closeNativeResumeDirectory(directory outputcap.Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}

func closeNativeResumeFile(file outputcap.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func closeNativeResumeEntry(entry outputcap.CurrentEntryReference) error {
	if entry == nil {
		return nil
	}
	return entry.Close()
}

func closeNativeResumeGuard(guard outputcap.PublicOperationGuard) error {
	if guard == nil {
		return nil
	}
	return guard.Close()
}
