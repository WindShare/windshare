package eventproject

import (
	"fmt"
	"testing"

	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestApplicationStopCausesKeepCancellationMeaningThroughWrappers(t *testing.T) {
	for _, cause := range []error{task.ErrCancelled, task.ErrApplicationClosed, task.ErrShareStopped} {
		failure, present := ClassifyError(fmt.Errorf("stop workflow: %w", cause))
		if !present || failure.Code() != appobs.FailureCanceled {
			t.Fatalf("local stop became a provider failure: cause=%v fact=%+v present=%v", cause, failure, present)
		}
	}
}
