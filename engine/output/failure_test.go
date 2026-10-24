package output

import (
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/engine/observation"
)

func TestFailureAnnotationsKeepPrimaryAndCleanupIndependent(t *testing.T) {
	primaryCause := errors.New("document provider rejected checkpoint")
	closeCause := errors.New("document grant release failed")
	fact, _ := observation.NewFailure(observation.FailureCheckpointStateIO)
	primary, ok := SealFailure(primaryCause, FailureDiagnostic{Stage: FailureCheckpoint, Failure: fact})
	if !ok {
		t.Fatal("valid provider failure rejected")
	}
	cleanup, _ := SealFailure(closeCause, FailureDiagnostic{Stage: FailureClose})
	joined := fmt.Errorf("provider operation: %w", errors.Join(primary, cleanup))
	diagnostic, ok := FindDiagnostic(joined)
	if !ok || diagnostic.Stage != FailureCheckpoint || diagnostic.Failure != fact {
		t.Fatalf("primary diagnostic lost: %+v", diagnostic)
	}
	selected := FailureForStage(joined, FailureClose)
	if !errors.Is(selected, closeCause) || errors.Is(selected, primaryCause) {
		t.Fatalf("cleanup includes unrelated work: %v", selected)
	}
	if !errors.Is(joined, primaryCause) || !errors.Is(joined, closeCause) {
		t.Fatal("provider causes lost")
	}
	if _, ok := DiagnosticFor(joined); ok {
		t.Fatal("direct classification traversed a wrapper")
	}
	if diagnostic, ok := DiagnosticFor(primary); !ok || diagnostic.Stage.String() != "checkpoint" {
		t.Fatal("explicit annotation lost")
	}
}

type cyclicFailure struct{}

func (*cyclicFailure) Error() string       { return "cycle" }
func (value *cyclicFailure) Unwrap() error { return value }

func TestFailureLookupIsBoundedAndDoesNotInventProviderEvidence(t *testing.T) {
	for _, cause := range []error{nil, errors.New("opaque"), &cyclicFailure{}} {
		if _, ok := FindDiagnostic(cause); ok {
			t.Fatal("invented annotation")
		}
		if got := FailureForStage(cause, FailureClose); got != nil {
			t.Fatalf("invented cleanup: %v", got)
		}
	}
	for _, diagnostic := range []FailureDiagnostic{{}, {Stage: 255}} {
		if _, ok := SealFailure(errors.New("cause"), diagnostic); ok {
			t.Fatal("invalid stage accepted")
		}
	}
	if _, ok := SealFailure(nil, FailureDiagnostic{Stage: FailureClose}); ok {
		t.Fatal("nil cause accepted")
	}
	if _, ok := DiagnosticFor((*failure)(nil)); ok {
		t.Fatal("nil envelope accepted")
	}
	if got := FailureForStage(errors.New("opaque"), 255); got != nil {
		t.Fatal("invalid stage accepted")
	}
	for stage := FailureDestinationBinding; stage <= FailureClose; stage++ {
		if !stage.Valid() || stage.String() == "" {
			t.Fatalf("missing stage %d", stage)
		}
	}
	if FailureStage(0).String() != "" {
		t.Fatal("zero stage invented a name")
	}
}

func TestFailureLookupPreservesCauseOrderAndEveryCloseFailure(t *testing.T) {
	first, _ := SealFailure(errors.New("publish"), FailureDiagnostic{Stage: FailureDurability})
	second, _ := SealFailure(errors.New("lookup"), FailureDiagnostic{Stage: FailureLookup})
	diagnostic, ok := FindDiagnostic(errors.Join(first, second))
	if !ok || diagnostic.Stage != FailureDurability {
		t.Fatal("stage order replaced cause order")
	}
	causeA, causeB := errors.New("close A"), errors.New("close B")
	closeA, _ := SealFailure(causeA, FailureDiagnostic{Stage: FailureClose})
	closeB, _ := SealFailure(causeB, FailureDiagnostic{Stage: FailureClose})
	selected := FailureForStage(errors.Join(first, errors.Join(closeA, closeB)), FailureClose)
	if !errors.Is(selected, causeA) || !errors.Is(selected, causeB) || errors.Is(selected, first) {
		t.Fatalf("independent release failures lost: %v", selected)
	}
}
