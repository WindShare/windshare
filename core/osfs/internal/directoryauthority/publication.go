package directoryauthority

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

// WithPublication pins ancestry once for observation, no-replace publication,
// and the namespace durability barrier. The guard ends before any checkpoint
// update or network wait so a slow transfer cannot lock the user's directory.
func (destination *fileDestination) WithPublication(
	ctx context.Context,
	owned fileexecution.OwnedFile,
	expectation fileexecution.FinalExpectation,
	operation func(fileexecution.FilePublication) error,
) error {
	object, err := checkpointmodel.ObjectIDFromBytes(expectation.ObjectIdentity().Bytes())
	if err != nil || owned == nil || owned.ObjectID() != object || operation == nil {
		return directoryBoundaryError(ctx, errors.Join(ErrInvalidClaim, err))
	}
	_, err = destination.withParent(ctx, func(parent outputcap.Directory) (fileexecution.FinalObservation, error) {
		publication := &filePublication{
			destination: destination, parent: parent, owned: owned, object: object, expectation: expectation,
		}
		defer publication.release()
		return fileexecution.FinalObservation{}, operation(publication)
	})
	return err
}

type filePublication struct {
	mu          sync.Mutex
	destination *fileDestination
	parent      outputcap.Directory
	owned       fileexecution.OwnedFile
	object      checkpointmodel.ObjectID
	expectation fileexecution.FinalExpectation
}

func (publication *filePublication) release() {
	publication.mu.Lock()
	defer publication.mu.Unlock()
	publication.parent = nil
}

func (publication *filePublication) validate(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidClaim
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if publication.parent == nil {
		return ErrAuthorityClosed
	}
	return nil
}

func (publication *filePublication) Observe(ctx context.Context) (fileexecution.FinalObservation, error) {
	publication.mu.Lock()
	defer publication.mu.Unlock()
	if err := publication.validate(ctx); err != nil {
		return fileexecution.FinalObservation{}, err
	}
	if publication.destination.objects != nil {
		return publication.destination.observeFinalAtParent(
			ctx, publication.parent, publication.object, publication.expectation,
		)
	}
	source, ok := publication.owned.(nativeOwnedSource)
	if !ok || source.NativeFile() == nil {
		return fileexecution.FinalObservation{}, ErrInvalidClaim
	}
	return publication.destination.observeFinalAgainstFile(publication.parent, source.NativeFile(), publication.expectation)
}

func (publication *filePublication) PublishNoReplace(ctx context.Context) (fileexecution.FinalObservation, error) {
	publication.mu.Lock()
	defer publication.mu.Unlock()
	if err := publication.validate(ctx); err != nil {
		return fileexecution.FinalObservation{}, err
	}
	destination, parent := publication.destination, publication.parent
	owned, object, expectation := publication.owned, publication.object, publication.expectation
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
}

func (publication *filePublication) Sync(ctx context.Context) error {
	publication.mu.Lock()
	defer publication.mu.Unlock()
	if err := publication.validate(ctx); err != nil {
		return err
	}
	return publication.parent.Sync()
}
