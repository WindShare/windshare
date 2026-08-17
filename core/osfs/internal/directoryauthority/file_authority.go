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
	FinalMatchesOwned(context.Context, checkpointmodel.ObjectID, uint64, outputcap.FileIdentity) (bool, error)
	PublishOwnedNoReplace(
		context.Context,
		checkpointmodel.ObjectID,
		uint64,
		outputcap.Directory,
		string,
	) (outputcap.ObservedFile, error)
}

type FileAuthority struct {
	directories *Authority
	objects     OwnedObjectAuthority
	sessionID   transfer.OutputSessionID
}

var _ fileexecution.DirectoryAuthority = (*FileAuthority)(nil)

func NewFileAuthority(
	directories *Authority,
	objects OwnedObjectAuthority,
	sessionID transfer.OutputSessionID,
) (*FileAuthority, error) {
	if directories == nil || objects == nil || sessionID.IsZero() {
		return nil, ErrInvalidConfiguration
	}
	return &FileAuthority{directories: directories, objects: objects, sessionID: sessionID}, nil
}

// NewLiveFileAuthority binds destinations for a live-only transaction. Public
// publication compares and links the retained stage handle directly, so there
// is no checkpoint object store or second ownership witness.
func NewLiveFileAuthority(
	directories *Authority,
	sessionID transfer.OutputSessionID,
) (*FileAuthority, error) {
	if directories == nil || sessionID.IsZero() {
		return nil, ErrInvalidConfiguration
	}
	return &FileAuthority{directories: directories, sessionID: sessionID}, nil
}

func (authority *FileAuthority) BindFile(
	ctx context.Context,
	file transfer.MaterializationFile,
	destinationPath transfer.OutputDestinationPath,
) (destination fileexecution.FileDestination, resultErr error) {
	defer func() {
		resultErr = directoryBoundaryError(ctx, resultErr)
	}()
	if authority == nil || authority.directories == nil || ctx == nil ||
		authority.sessionID.IsZero() || file.Target().OutputSessionID() != authority.sessionID ||
		!file.ArtifactPath().Valid() || !destinationPath.Valid() || destinationPath.IsSessionRoot() {
		return nil, ErrInvalidClaim
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directories := authority.directories
	artifactPath := file.ArtifactPath().String()
	canonicalDestination := destinationPath.String()
	locator, err := directories.canonicalLocator(canonicalDestination)
	if err != nil || !locator.valid() || locator.isRoot() {
		return nil, errors.Join(ErrInvalidClaim, err)
	}
	directories.gate.RLock()
	defer directories.gate.RUnlock()
	parentID := ClaimID(0)
	parentPath := ""
	if file.ParentMaterialization().Valid() {
		directories.mu.Lock()
		parentID = directories.admissions[string(file.ParentMaterialization().Admission().Bytes())]
		directories.mu.Unlock()
		lineage, lineageErr := directories.readyLineage(parentID)
		if lineageErr != nil || len(lineage) == 0 {
			return nil, errors.Join(ErrParentUnavailable, lineageErr)
		}
		parentPath = lineage[len(lineage)-1].claim.locator.canonicalPath
	}
	if !validateImmediateChild(parentPath, locator.canonicalPath) ||
		artifactPath == "" || file.ExpectedSize() != file.Descriptor().ExactSize() ||
		file.Target().Descriptor() != file.Descriptor() || file.Target().ExactSize() != file.ExpectedSize() ||
		file.Target().Locator().Kind() != transfer.MaterializationPathLocator ||
		file.Target().Locator().CanonicalPath() != artifactPath {
		return nil, ErrInvalidClaim
	}
	return &fileDestination{
		authority: directories,
		objects:   authority.objects,
		parentID:  parentID,
		target:    file.Target(),
		leaf:      locator.leaf,
	}, nil
}

type nativeOwnedSource interface {
	NativeFile() outputcap.MutableFile
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

// WithExactParent gives stage creation one freshly revalidated parent handle.
// The callback cannot retain the borrowed capability beyond this mutation cut.
func (destination *fileDestination) WithExactParent(
	ctx context.Context,
	operation func(outputcap.Directory) error,
) error {
	if operation == nil {
		return ErrInvalidClaim
	}
	_, err := destination.withParent(ctx, func(parent outputcap.Directory) (fileexecution.FinalObservation, error) {
		return fileexecution.FinalObservation{}, operation(parent)
	})
	return err
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

func (destination *fileDestination) observeFinalForOwned(
	ctx context.Context,
	owned fileexecution.OwnedFile,
	expectation fileexecution.FinalExpectation,
) (fileexecution.FinalObservation, error) {
	if destination.objects != nil {
		return destination.ObserveFinal(ctx, expectation)
	}
	source, ok := owned.(nativeOwnedSource)
	if !ok || source.NativeFile() == nil {
		return fileexecution.FinalObservation{}, directoryBoundaryError(ctx, ErrInvalidClaim)
	}
	return destination.withParent(ctx, func(parent outputcap.Directory) (fileexecution.FinalObservation, error) {
		return destination.observeFinalAgainstFile(parent, source.NativeFile(), expectation)
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
			if destination.objects != nil {
				return destination.observeFinalAtParent(ctx, parent, object, expectation)
			}
			source, ok := owned.(nativeOwnedSource)
			if !ok || source.NativeFile() == nil {
				return fileexecution.FinalObservation{}, ErrInvalidClaim
			}
			return destination.observeFinalAgainstFile(parent, source.NativeFile(), expectation)
		}
		var linked outputcap.ObservedFile
		var publishErr error
		if destination.objects != nil {
			linked, publishErr = destination.objects.PublishOwnedNoReplace(
				ctx, object, expectation.ExactSize(), parent, destination.leaf,
			)
		} else {
			source, ok := owned.(nativeOwnedSource)
			if !ok || source.NativeFile() == nil {
				return fileexecution.FinalObservation{}, ErrInvalidClaim
			}
			linked, publishErr = parent.LinkFileNoReplace(source.NativeFile(), destination.leaf)
		}
		if publishErr == nil && linked == nil {
			publishErr = outputcap.ErrUnsafeNamespace
		}
		linkedCloseErr := closeRuntimeFile(linked)
		var observed fileexecution.FinalObservation
		var observeErr error
		if destination.objects != nil {
			observed, observeErr = destination.observeFinalAtParent(ctx, parent, object, expectation)
		} else {
			source := owned.(nativeOwnedSource)
			observed, observeErr = destination.observeFinalAgainstFile(parent, source.NativeFile(), expectation)
		}
		return observed, errors.Join(publishErr, linkedCloseErr, observeErr)
	})
}

func (destination *fileDestination) ObserveOwnedFinal(
	ctx context.Context,
	owned fileexecution.OwnedFile,
	expectation fileexecution.FinalExpectation,
) (fileexecution.FinalObservation, error) {
	return destination.observeFinalForOwned(ctx, owned, expectation)
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
	if destination == nil || destination.authority == nil ||
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

func (destination *fileDestination) observeFinalAgainstFile(
	parent outputcap.Directory,
	owned outputcap.FileIdentity,
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
	final, err := parent.OpenObservedFile(destination.leaf, false)
	if err != nil || final == nil {
		return fileexecution.FinalObservation{}, errors.Join(err, outputcap.ErrUnsafeNamespace, closeRuntimeFile(final))
	}
	same, sameErr := final.SameFile(owned)
	size, sizeErr := final.Size()
	if sameErr != nil || sizeErr != nil {
		return fileexecution.FinalObservation{}, errors.Join(sameErr, sizeErr, final.Close())
	}
	if !same || size != expectation.ExactSize() {
		return finalObservationWithClose(fileexecution.FinalCollision, final)
	}
	return finalObservationWithClose(fileexecution.FinalOwnedExact, final)
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
	final, err := parent.OpenObservedFile(destination.leaf, false)
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
	// Native identity and exact size prove publication. Display metadata such as
	// mtime is deliberately absent from this authority decision because platforms
	// may round or reject it after authenticated bytes are already correct.
	return finalObservationWithClose(fileexecution.FinalOwnedExact, final)
}

func finalObservationWithClose(
	condition fileexecution.FinalCondition,
	file outputcap.FileIdentity,
) (fileexecution.FinalObservation, error) {
	observation, err := fileexecution.ObserveFinal(condition)
	return observation, errors.Join(err, closeRuntimeFile(file))
}

func closeRuntimeFile(file outputcap.FileIdentity) error {
	if file == nil {
		return nil
	}
	return file.Close()
}
