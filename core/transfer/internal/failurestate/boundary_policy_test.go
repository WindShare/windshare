package failurestate_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/internal/failurestate"
)

func TestClassifiedPolicySurvivesPublicBoundaryReduction(t *testing.T) {
	session, _ := fault.NewSession(fault.ScopeSessionTerminal, fault.SessionTransport)
	output, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputStateIO)
	source, _ := fault.NewSource(fault.ScopeFileLocal, fault.SourcePermanent)
	ctx := context.Background()
	for _, value := range []fault.Fault{session, output, source} {
		for _, cancellation := range []error{context.Canceled, context.DeadlineExceeded} {
			t.Run(fmt.Sprintf("%s/%v", value, cancellation), func(t *testing.T) {
				boundary := fmt.Errorf("collaborator: %w", fault.Wrap(value, errors.Join(
					errors.New("classified failure"), cancellation,
				)))
				for _, route := range []struct {
					name string
					err  error
				}{
					{"direct", boundary},
					{"contextual reduction", fault.ReduceBoundaryErrors(ctx, boundary)},
					{"completed reduction", fault.ReduceBoundaryErrorSet(boundary)},
				} {
					t.Run(route.name, func(t *testing.T) {
						for _, result := range []fault.BoundaryResult{
							fault.NormalizeBoundary(ctx, route.err), fault.NormalizeBoundaryError(route.err),
						} {
							got, valid := result.Fault()
							if result.Kind() != fault.BoundaryFailed || !valid || got != value {
								t.Fatalf("public boundary = (%v, %v), want fault %v", result.Kind(), got, value)
							}
						}
						internal := failurestate.Normalize(ctx, route.err, nil)
						if internal.Canceled() || internal.Fault() != value || failurestate.IsCancellation(ctx, route.err) {
							t.Fatalf("internal boundary = %v, want fault %v without cancellation", internal, value)
						}
						if !errors.Is(internal.Diagnostic(), cancellation) {
							t.Fatal("classification discarded cancellation diagnostics")
						}
					})
				}
				if reduced := fault.ReduceBoundaryErrors(ctx, boundary, cancellation); reduced != cancellation {
					t.Fatalf("independent sibling cancellation = %v, want %v", reduced, cancellation)
				}
			})
		}
	}
}

func TestCallerDeadlineWinsOverClassifiedBoundary(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Time{})
	defer cancel()
	value, _ := fault.NewSession(fault.ScopeSessionTerminal, fault.SessionTransport)
	boundary := fault.Wrap(value, context.Canceled)
	if fault.NormalizeBoundary(ctx, boundary).Kind() != fault.BoundaryCanceled ||
		fault.ReduceBoundaryErrors(ctx, boundary) != context.DeadlineExceeded {
		t.Fatal("public boundary ignored the caller's deadline")
	}
	if got := failurestate.Normalize(ctx, boundary, nil); !got.Canceled() || got.Cancellation() != context.DeadlineExceeded {
		t.Fatalf("internal boundary = %v, want caller deadline", got)
	}
}
