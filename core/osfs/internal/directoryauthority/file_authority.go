package directoryauthority

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

// OwnedObjectAuthority keeps private stage and anchor handles behind their
// checkpoint namespace. Directory authority receives only exact comparison and
// no-replace publication operations for an already-bound object ID.
type OwnedObjectAuthority interface {
	FinalMatchesOwned(context.Context, checkpointmodel.ObjectID, uint64, outputcap.File) (bool, error)
	PublishOwnedNoReplace(
		context.Context,
		checkpointmodel.ObjectID,
		uint64,
		outputcap.Directory,
		string,
	) (outputcap.File, error)
}

type FileAuthority struct {
	directories *Authority
	objects     OwnedObjectAuthority
}

var _ fileexecution.DirectoryAuthority = (*FileAuthority)(nil)

func NewFileAuthority(directories *Authority, objects OwnedObjectAuthority) (*FileAuthority, error) {
	if directories == nil || objects == nil {
		return nil, ErrInvalidConfiguration
	}
	return &FileAuthority{directories: directories, objects: objects}, nil
}

func (authority *FileAuthority) BindFile(
	ctx context.Context,
	file transfer.MaterializationFile,
) (destination fileexecution.FileDestination, resultErr error) {
	defer func() {
		resultErr = directoryBoundaryError(ctx, resultErr)
	}()
	if authority == nil || authority.directories == nil || authority.objects == nil || ctx == nil ||
		file.Path == "" || file.ParentAdmission.IsZero() {
		return nil, ErrInvalidClaim
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directories := authority.directories
	locator, err := directories.canonicalLocator(file.Path)
	if err != nil || !locator.valid() || locator.isRoot() {
		return nil, errors.Join(ErrInvalidClaim, err)
	}
	directories.gate.RLock()
	defer directories.gate.RUnlock()
	directories.mu.Lock()
	parentID := directories.admissions[string(file.ParentAdmission.Bytes())]
	directories.mu.Unlock()
	lineage, err := directories.readyLineage(parentID)
	if err != nil || len(lineage) == 0 {
		return nil, errors.Join(ErrParentUnavailable, err)
	}
	parent := lineage[len(lineage)-1].claim
	if !validateImmediateChild(parent.locator.canonicalPath, locator.canonicalPath) ||
		file.Path == "" || file.ExpectedSize != file.Descriptor.ExactSize() ||
		file.Target.Descriptor() != file.Descriptor || file.Target.ExactSize() != file.ExpectedSize ||
		file.Target.Locator().Kind() != transfer.MaterializationPathLocator ||
		file.Target.Locator().CanonicalPath() != file.Path {
		return nil, ErrInvalidClaim
	}
	return &fileDestination{
		authority: directories,
		objects:   authority.objects,
		parentID:  parentID,
		target:    file.Target,
		leaf:      locator.leaf,
	}, nil
}

type fileDestination struct {
	mu sync.Mutex

	authority *Authority
	objects   OwnedObjectAuthority
	parentID  ClaimID
	target    transfer.FileMaterializationTarget
	leaf      string
	closed    bool
}

var _ fileexecution.FileDestination = (*fileDestination)(nil)

func (destination *fileDestination) Target() transfer.FileMaterializationTarget {
	if destination == nil {
		return transfer.FileMaterializationTarget{}
	}
	return destination.target
}

func (destination *fileDestination) ObserveFinalPresence(
	ctx context.Context,
) (fileexecution.FinalObservation, error) {
	return destination.withParent(ctx, func(parent outputcap.Directory) (fileexecution.FinalObservation, error) {
		kind, exact, err := parent.ClassifyExactEntry(destination.leaf)
		if err != nil {
			return fileexecution.FinalObservation{}, err
		}
		if !exact {
			return fileexecution.ObserveFinal(fileexecution.FinalUnsafe)
		}
		if kind == outputcap.EntryAbsent {
			return fileexecution.ObserveFinal(fileexecution.FinalAbsent)
		}
		return fileexecution.ObserveFinal(fileexecution.FinalCollision)
	})
}

func (destination *fileDestination) ObserveFinal(
	ctx context.Context,
	expectation fileexecution.FinalExpectation,
) (fileexecution.FinalObservation, error) {
	object, err := checkpointmodel.ObjectIDFromBytes(expectation.ObjectIdentity().Bytes())
	if err != nil {
		return fileexecution.FinalObservation{}, directoryBoundaryError(ctx, errors.Join(ErrInvalidClaim, err))
	}
	return destination.withParent(ctx, func(parent outputcap.Directory) (fileexecution.FinalObservation, error) {
		return destination.observeFinalAtParent(ctx, parent, object, expectation)
	})
}

func (destination *fileDestination) PublishNoReplace(
	ctx context.Context,
	owned fileexecution.OwnedFile,
	expectation fileexecution.FinalExpectation,
) (fileexecution.FinalObservation, error) {
	object, err := checkpointmodel.ObjectIDFromBytes(expectation.ObjectIdentity().Bytes())
	if err != nil || owned == nil || owned.ObjectID() != object {
		return fileexecution.FinalObservation{}, directoryBoundaryError(ctx, errors.Join(ErrInvalidClaim, err))
	}
	return destination.withParent(ctx, func(parent outputcap.Directory) (fileexecution.FinalObservation, error) {
		kind, exact, classifyErr := parent.ClassifyExactEntry(destination.leaf)
		if classifyErr != nil {
			return fileexecution.FinalObservation{}, classifyErr
		}
		if !exact {
			return fileexecution.ObserveFinal(fileexecution.FinalUnsafe)
		}
		if kind != outputcap.EntryAbsent {
			return destination.observeFinalAtParent(ctx, parent, object, expectation)
		}
		linked, publishErr := destination.objects.PublishOwnedNoReplace(
			ctx, object, expectation.ExactSize(), parent, destination.leaf,
		)
		if publishErr == nil && linked == nil {
			publishErr = outputcap.ErrUnsafeNamespace
		}
		linkedCloseErr := closeRuntimeFile(linked)
		observed, observeErr := destination.observeFinalAtParent(ctx, parent, object, expectation)
		return observed, errors.Join(publishErr, linkedCloseErr, observeErr)
	})
}

func (destination *fileDestination) SyncFinalParent(ctx context.Context) error {
	_, err := destination.withParent(ctx, func(parent outputcap.Directory) (fileexecution.FinalObservation, error) {
		return fileexecution.FinalObservation{}, parent.Sync()
	})
	return err
}

func (destination *fileDestination) Close() error {
	if destination == nil {
		return nil
	}
	destination.mu.Lock()
	destination.closed = true
	destination.mu.Unlock()
	return nil
}

func (destination *fileDestination) withParent(
	ctx context.Context,
	operation func(outputcap.Directory) (fileexecution.FinalObservation, error),
) (observation fileexecution.FinalObservation, resultErr error) {
	defer func() {
		resultErr = directoryBoundaryError(ctx, resultErr)
	}()
	if destination == nil || destination.authority == nil || destination.objects == nil ||
		ctx == nil || operation == nil {
		return fileexecution.FinalObservation{}, ErrInvalidClaim
	}
	if err := ctx.Err(); err != nil {
		return fileexecution.FinalObservation{}, err
	}
	destination.mu.Lock()
	defer destination.mu.Unlock()
	if destination.closed {
		return fileexecution.FinalObservation{}, ErrAuthorityClosed
	}
	destination.authority.gate.RLock()
	defer destination.authority.gate.RUnlock()
	parent, cleanup, err := destination.authority.openGuardedDirectory(destination.parentID)
	if err != nil {
		return fileexecution.FinalObservation{}, err
	}
	observation, operationErr := operation(parent)
	return observation, errors.Join(operationErr, cleanup())
}

func (destination *fileDestination) observeFinalAtParent(
	ctx context.Context,
	parent outputcap.Directory,
	object checkpointmodel.ObjectID,
	expectation fileexecution.FinalExpectation,
) (fileexecution.FinalObservation, error) {
	kind, exact, err := parent.ClassifyExactEntry(destination.leaf)
	if err != nil {
		return fileexecution.FinalObservation{}, err
	}
	if !exact {
		return fileexecution.ObserveFinal(fileexecution.FinalUnsafe)
	}
	if kind == outputcap.EntryAbsent {
		return fileexecution.ObserveFinal(fileexecution.FinalAbsent)
	}
	if kind != outputcap.EntryRegularFile {
		return fileexecution.ObserveFinal(fileexecution.FinalUnsafe)
	}
	final, err := parent.OpenFile(destination.leaf, false, false)
	if err != nil || final == nil {
		return fileexecution.FinalObservation{}, errors.Join(err, outputcap.ErrUnsafeNamespace, closeRuntimeFile(final))
	}
	owned, compareErr := destination.objects.FinalMatchesOwned(
		ctx, object, expectation.ExactSize(), final,
	)
	if compareErr != nil {
		return fileexecution.FinalObservation{}, errors.Join(compareErr, final.Close())
	}
	if !owned {
		return finalObservationWithClose(fileexecution.FinalCollision, final)
	}
	metadata, metadataErr := final.MetadataMatches(expectation.ExactSize(), expectation.ModifiedTime())
	if metadataErr != nil {
		return fileexecution.FinalObservation{}, errors.Join(metadataErr, final.Close())
	}
	if !metadata {
		return finalObservationWithClose(fileexecution.FinalOwnedMetadataMismatch, final)
	}
	return finalObservationWithClose(fileexecution.FinalOwnedExact, final)
}

func finalObservationWithClose(
	condition fileexecution.FinalCondition,
	file outputcap.File,
) (fileexecution.FinalObservation, error) {
	observation, err := fileexecution.ObserveFinal(condition)
	return observation, errors.Join(err, closeRuntimeFile(file))
}

func closeRuntimeFile(file outputcap.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}
