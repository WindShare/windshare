package directoryauthority

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestDirectoryBoundaryErrorClosesNativeTaxonomy(t *testing.T) {
	t.Parallel()

	outputFault := func(code fault.OutputCode) fault.Fault {
		value, err := fault.NewOutput(fault.ScopeOutputPause, code)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	for name, test := range map[string]struct {
		cause error
		want  fault.Fault
	}{
		"contract":  {cause: ErrInvalidClaim, want: fault.DependencyContractFault()},
		"ambiguous": {cause: ErrMutationAmbiguous, want: outputFault(fault.OutputMutationAmbiguous)},
		"namespace": {cause: outputcap.ErrUnsafeNamespace, want: outputFault(fault.OutputNamespaceUnsafe)},
		"ownership": {cause: ErrAuthorityClosed, want: outputFault(fault.OutputOwnership)},
		"state I/O": {cause: errors.New("native write failed"), want: outputFault(fault.OutputStateIO)},
	} {
		t.Run(name, func(t *testing.T) {
			actual := directoryBoundaryError(context.Background(), test.cause)
			var boundary *fault.BoundaryError
			if !errors.As(actual, &boundary) || boundary.Fault() != test.want || !errors.Is(actual, test.cause) {
				t.Fatalf("boundary error = %v, want %v", actual, test.want)
			}
		})
	}
}

func TestDirectoryBoundaryErrorKeepsCancellationAndTypedFaultDistinct(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := directoryBoundaryError(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context = %v", err)
	}
	if err := directoryBoundaryError(context.Background(), fmt.Errorf("deadline: %w", context.DeadlineExceeded)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline = %v", err)
	}
	if err := directoryBoundaryError(context.Background(), nil); err != nil {
		t.Fatalf("success = %v", err)
	}

	value, err := fault.NewOutput(fault.ScopeOutputPause, fault.OutputOwnership)
	if err != nil {
		t.Fatal(err)
	}
	typed := fault.Wrap(value, ErrAuthorityClosed)
	if actual := directoryBoundaryError(context.Background(), typed); actual != typed {
		t.Fatal("exact typed boundary was rewrapped")
	}
	wrapped := fmt.Errorf("adapter: %w", typed)
	actual := directoryBoundaryError(context.Background(), wrapped)
	var boundary *fault.BoundaryError
	if !errors.As(actual, &boundary) || boundary.Fault() != value || actual == wrapped {
		t.Fatal("wrapped typed boundary was not projected as one closed result")
	}
}
