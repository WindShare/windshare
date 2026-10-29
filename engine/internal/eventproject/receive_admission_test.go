package eventproject

import (
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/engine/internal/receive"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestMissingResumeOperationKeepsItsKnownFailureAcrossProjection(t *testing.T) {
	failure := receive.Failure{Code: receive.FailureRetainedOutputMissing, Cause: receive.ErrRetainedOutputMissing}
	for _, cause := range []error{
		failure,
		fmt.Errorf("receive admission: %w", failure),
		errors.Join(failure, receive.ErrRetainedOutputMissing),
	} {
		projected, present := ClassifyError(cause)
		key, known := projected.MessageKey()
		if !present || !known || projected.Code() != appobs.FailureRetainedOutputMissing || key != appobs.MessageRetainedOutputMissing {
			t.Fatalf("missing resume operation became an unknown error: %+v present=%t", projected, present)
		}
	}
}
