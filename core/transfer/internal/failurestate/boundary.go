package failurestate

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/transfer/fault"
)

type boundaryFaultClassifier func(error) (fault.Fault, bool)

// Normalize admits an immediate collaborator result once. Policy code receives Failure and therefore
// cannot recover authority from wrapping shape or joined error topology.
func Normalize(
	ctx context.Context,
	err error,
	classify boundaryFaultClassifier,
) *Failure {
	if existing := ContextCause(ctx); existing != nil {
		return existing
	}
	if cancellation := boundaryCancellation(ctx, err); cancellation != cancellationNone {
		diagnostic := err
		if diagnostic == nil && ctx != nil {
			diagnostic = context.Cause(ctx)
		}
		return &Failure{
			cancellation: cancellation, diagnostic: diagnostic,
		}
	}
	if existing, ok := Admit(err); ok {
		return existing
	}
	if err == nil {
		return nil
	}
	var normalized *fault.BoundaryError
	if errors.As(err, &normalized) && normalized != nil && normalized.Fault().Valid() {
		return New(normalized.Fault(), err)
	}
	if classify != nil {
		if value, known := classify(err); known {
			return New(value, err)
		}
	}
	return New(fault.DependencyContractFault(), err)
}

func boundaryCancellation(ctx context.Context, err error) cancellationKind {
	if ctx != nil {
		switch ctx.Err() {
		case context.Canceled:
			return cancellationCanceled
		case context.DeadlineExceeded:
			return cancellationDeadline
		}
	}
	if err == nil {
		return cancellationNone
	}
	if errors.Is(err, context.Canceled) {
		return cancellationCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return cancellationDeadline
	}
	return cancellationNone
}

func FromCancellation(ctx context.Context, diagnostic error) *Failure {
	if existing, ok := Admit(diagnostic); ok {
		return existing
	}
	if existing := ContextCause(ctx); existing != nil {
		return existing
	}
	cancellation := boundaryCancellation(ctx, diagnostic)
	if cancellation == cancellationNone {
		return DependencyContract(diagnostic)
	}
	if diagnostic == nil && ctx != nil {
		diagnostic = context.Cause(ctx)
	}
	return &Failure{
		cancellation: cancellation, diagnostic: diagnostic,
	}
}

func ContextCause(ctx context.Context) *Failure {
	if ctx == nil {
		return nil
	}
	failure, _ := Admit(context.Cause(ctx))
	return failure
}

// BoundaryError projects an internal normalized reduction back across an
// error-returning port without leaking internal failure provenance.
func BoundaryError(failure *Failure, diagnostic error) error {
	if failure == nil {
		return fault.Wrap(fault.DependencyContractFault(), diagnostic)
	}
	if failure.Canceled() {
		if cancellation := failure.cancellation.cause(); cancellation != nil {
			return cancellation
		}
		return context.Canceled
	}
	return fault.Wrap(failure.value, diagnostic)
}

// IsCancellation inspects an immediate collaborator result before admitting its policy.
func IsCancellation(ctx context.Context, err error) bool {
	return boundaryCancellation(ctx, err) != cancellationNone
}
