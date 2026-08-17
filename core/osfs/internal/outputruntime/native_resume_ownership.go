package outputruntime

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func observeOrdinaryResumeFinal(
	ctx context.Context,
	topLevel *destinationauthority.TopLevelReservation,
	store *checkpointstore.FileExecutionStore,
	record checkpointmodel.Record,
) (result fileexecution.FinalObservation, resultErr error) {
	if ctx == nil || topLevel == nil || store == nil || !record.Valid() {
		return fileexecution.FinalObservation{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return fileexecution.FinalObservation{}, err
	}
	physical, err := destinationauthority.PhysicalArtifactPath(
		record.CanonicalPath(), topLevel.ReservedEntry(),
	)
	if err != nil || physical == "" {
		return fileexecution.FinalObservation{}, errors.Join(err, transfer.ErrInvalidOutputBinding)
	}
	guard, err := topLevel.AcquirePublicOperationGuard()
	if err != nil || guard == nil || guard.Root() == nil {
		return fileexecution.FinalObservation{}, errors.Join(err, ErrNativeResumeOwnershipUnknown)
	}
	defer func() { resultErr = errors.Join(resultErr, guard.Close()) }()

	components := strings.Split(physical, "/")
	parent, opened, terminal, settled, err := openOrdinaryResumeFinalParent(
		guard.Root(), components[:len(components)-1],
	)
	defer func() {
		resultErr = errors.Join(resultErr, closeOrdinaryResumeDirectories(opened))
	}()
	if settled || err != nil {
		return terminal, err
	}
	return observeOrdinaryResumeFinalLeaf(
		ctx, parent, components[len(components)-1], store, record,
	)
}

func openOrdinaryResumeFinalParent(
	root outputcap.Directory,
	components []string,
) (
	outputcap.Directory,
	[]outputcap.Directory,
	fileexecution.FinalObservation,
	bool,
	error,
) {
	current := root
	opened := make([]outputcap.Directory, 0, len(components))
	for _, component := range components {
		kind, exact, err := current.ClassifyExactEntry(component)
		if err != nil || !exact {
			observation, observationErr := finalObservation(fileexecution.FinalUnsafe, err)
			return nil, opened, observation, true, observationErr
		}
		if kind == outputcap.EntryAbsent {
			observation, observationErr := finalObservation(fileexecution.FinalAbsent, nil)
			return nil, opened, observation, true, observationErr
		}
		if kind != outputcap.EntryDirectory {
			observation, observationErr := finalObservation(fileexecution.FinalCollision, nil)
			return nil, opened, observation, true, observationErr
		}
		reference, err := current.OpenEntry(component)
		if err != nil || reference == nil || reference.Kind() != outputcap.EntryDirectory {
			observation, observationErr := finalObservation(
				fileexecution.FinalUnsafe, errors.Join(err, closeNativeResumeEntry(reference)),
			)
			return nil, opened, observation, true, observationErr
		}
		child, childErr := current.OpenPinnedDirectory(reference, false)
		unchanged, matchErr := current.EntryMatches(component, reference)
		closeErr := reference.Close()
		if childErr != nil || matchErr != nil || closeErr != nil || !unchanged || child == nil {
			observation, observationErr := finalObservation(
				fileexecution.FinalUnsafe,
				errors.Join(childErr, matchErr, closeErr, closeNativeResumeDirectory(child)),
			)
			return nil, opened, observation, true, observationErr
		}
		opened = append(opened, child)
		current = child
	}
	return current, opened, fileexecution.FinalObservation{}, false, nil
}

func observeOrdinaryResumeFinalLeaf(
	ctx context.Context,
	parent outputcap.Directory,
	leaf string,
	store *checkpointstore.FileExecutionStore,
	record checkpointmodel.Record,
) (result fileexecution.FinalObservation, resultErr error) {
	kind, exact, err := parent.ClassifyExactEntry(leaf)
	if err != nil || !exact {
		return finalObservation(fileexecution.FinalUnsafe, err)
	}
	if kind == outputcap.EntryAbsent {
		return finalObservation(fileexecution.FinalAbsent, nil)
	}
	if kind != outputcap.EntryRegularFile {
		return finalObservation(fileexecution.FinalCollision, nil)
	}
	reference, err := parent.OpenEntry(leaf)
	if err != nil || reference == nil || reference.Kind() != outputcap.EntryRegularFile {
		return finalObservation(
			fileexecution.FinalUnsafe, errors.Join(err, closeNativeResumeEntry(reference)),
		)
	}
	defer func() { resultErr = errors.Join(resultErr, reference.Close()) }()
	final, err := parent.OpenObservedFile(leaf, false)
	if err != nil || final == nil {
		return finalObservation(
			fileexecution.FinalUnsafe, errors.Join(err, closeNativeResumeFile(final)),
		)
	}
	defer func() { resultErr = errors.Join(resultErr, final.Close()) }()
	unchanged, err := parent.EntryMatches(leaf, reference)
	if err != nil || !unchanged {
		return finalObservation(fileexecution.FinalUnsafe, err)
	}
	matches, err := store.FinalMatchesOwned(
		ctx, record.OwnedObjectID(), record.ExactSize(), final,
	)
	if err != nil {
		return finalObservation(fileexecution.FinalUnsafe, err)
	}
	unchanged, err = parent.EntryMatches(leaf, reference)
	if err != nil || !unchanged {
		return finalObservation(fileexecution.FinalUnsafe, err)
	}
	if !matches {
		return finalObservation(fileexecution.FinalCollision, nil)
	}
	return finalObservation(fileexecution.FinalOwnedExact, nil)
}

func closeOrdinaryResumeDirectories(opened []outputcap.Directory) error {
	var resultErr error
	for _, directory := range slices.Backward(opened) {
		resultErr = errors.Join(resultErr, directory.Close())
	}
	return resultErr
}

func finalObservation(
	condition fileexecution.FinalCondition,
	err error,
) (fileexecution.FinalObservation, error) {
	observation, observationErr := fileexecution.ObserveFinal(condition)
	return observation, errors.Join(err, observationErr)
}

func closeNativeResumeDirectory(directory outputcap.Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}

func closeNativeResumeFile(file outputcap.ObservedFile) error {
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
