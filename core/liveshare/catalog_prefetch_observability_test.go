package liveshare

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestRootPrefetchDecisionNamesAreClosed(t *testing.T) {
	decisions := []struct {
		decision RootPrefetchDecision
		want     string
	}{
		{RootPrefetchAttemptStarted, "attempt-started"},
		{RootPrefetchYieldedToDemand, "yielded-to-demand"},
		{RootPrefetchRetryScheduled, "retry-scheduled"},
		{RootPrefetchCommitted, "committed"},
		{RootPrefetchBudgetFailed, "budget-failed"},
		{RootPrefetchScanFailed, "scan-failed"},
		{RootPrefetchStopped, "stopped"},
		{RootPrefetchDecision(255), "unknown"},
	}
	for _, test := range decisions {
		if got := test.decision.String(); got != test.want {
			t.Fatalf("decision %d string = %q, want %q", test.decision, got, test.want)
		}
	}
}

type singleUnwrapError struct{ err error }

func (failure singleUnwrapError) Error() string { return "single wrapped" }
func (failure singleUnwrapError) Unwrap() error { return failure.err }

type sliceUnwrapError struct{ errs []error }

func (failure sliceUnwrapError) Error() string   { return "slice wrapped" }
func (failure sliceUnwrapError) Unwrap() []error { return failure.errs }

type panicUnwrapError struct{}

func (panicUnwrapError) Error() string { return "panic unwrap" }
func (panicUnwrapError) Unwrap() error { panic("faulty unwrapper") }

func TestRootPrefetchFailureDecisionClassification(t *testing.T) {
	if got := rootPrefetchFailureDecision(nil); got != RootPrefetchScanFailed {
		t.Fatalf("nil error failure decision = %v, want scan-failed", got)
	}
	if got := rootPrefetchFailureDecision(catalog.ErrBudgetExceeded); got != RootPrefetchBudgetFailed {
		t.Fatalf("direct budget error failure decision = %v, want budget-failed", got)
	}
	if got := rootPrefetchFailureDecision(singleUnwrapError{err: catalog.ErrBudgetExceeded}); got != RootPrefetchBudgetFailed {
		t.Fatalf("single wrapped budget error failure decision = %v, want budget-failed", got)
	}
	if got := rootPrefetchFailureDecision(sliceUnwrapError{errs: []error{errors.New("other"), catalog.ErrBudgetExceeded}}); got != RootPrefetchBudgetFailed {
		t.Fatalf("slice wrapped budget error failure decision = %v, want budget-failed", got)
	}
	if got := rootPrefetchFailureDecision(panicUnwrapError{}); got != RootPrefetchScanFailed {
		t.Fatalf("panic unwrap error failure decision = %v, want scan-failed", got)
	}
	if got := rootPrefetchFailureDecision(errors.New("arbitrary error")); got != RootPrefetchScanFailed {
		t.Fatalf("arbitrary error failure decision = %v, want scan-failed", got)
	}
}

func TestTraceRootPrefetchIsOptionalAndPanicIsolated(t *testing.T) {
	traceRootPrefetch(nil, RootPrefetchTrace{})
	traceRootPrefetch(
		RootPrefetchTraceFunc(func(RootPrefetchTrace) { panic("diagnostic failure") }),
		RootPrefetchTrace{},
	)

	called := false
	traceRootPrefetch(RootPrefetchTraceFunc(func(RootPrefetchTrace) { called = true }), RootPrefetchTrace{})
	if !called {
		t.Fatal("explicit root prefetch tracer was not called")
	}
}
