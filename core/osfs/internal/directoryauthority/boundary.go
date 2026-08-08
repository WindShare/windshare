package directoryauthority

import (
	"context"
	"errors"
	"reflect"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer/fault"
)

// directoryBoundaryError closes the native filesystem taxonomy before it
// crosses into outputsession. Mutation evidence chooses scope here; downstream
// settlement must not rediscover authority by walking the native cause graph.
func directoryBoundaryError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if exact, ok := exactBoundaryError(err); ok && exact != nil && exact.Fault().Valid() {
		return exact
	}
	var normalized *fault.BoundaryError
	if errors.As(err, &normalized) && normalized != nil && normalized.Fault().Valid() {
		return fault.Wrap(normalized.Fault(), err)
	}

	var value fault.Fault
	switch {
	case errors.Is(err, ErrInvalidClaim), errors.Is(err, ErrClaimConflict),
		errors.Is(err, ErrParentUnavailable):
		value = fault.DependencyContractFault()
	case errors.Is(err, ErrMutationAmbiguous):
		value, _ = fault.NewOutput(fault.ScopeOutputPause, fault.OutputMutationAmbiguous)
	case errors.Is(err, ErrEntryCollision), errors.Is(err, ErrPlatformEquivalentLocator),
		errors.Is(err, outputcap.ErrNamespaceCollision), errors.Is(err, outputcap.ErrUnsafeNamespace):
		value, _ = fault.NewOutput(fault.ScopeOutputPause, fault.OutputNamespaceUnsafe)
	case errors.Is(err, ErrAuthorityClosed), errors.Is(err, ErrRetainedAuthorityChanged):
		value, _ = fault.NewOutput(fault.ScopeOutputPause, fault.OutputOwnership)
	default:
		value, _ = fault.NewOutput(fault.ScopeOutputPause, fault.OutputStateIO)
	}
	return fault.Wrap(value, err)
}

// exactBoundaryError admits the authority-bearing dynamic type before using
// errors.As. Outer wrappers remain diagnostic causes and are reprojected once.
func exactBoundaryError(err error) (*fault.BoundaryError, bool) {
	if err == nil || reflect.TypeOf(err) != reflect.TypeFor[*fault.BoundaryError]() {
		return nil, false
	}
	var admitted *fault.BoundaryError
	if !errors.As(err, &admitted) {
		return nil, false
	}
	return admitted, true
}
