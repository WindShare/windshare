package v2peer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/sessionruntime"
)

func TestReceiverAttemptPublishedGraphsAreImmutableOwnedSnapshots(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	genuine := errors.New("immutable retained receiver failure")
	original := errors.Join(context.Canceled, genuine, ErrProtocol)
	operation.terminateCause = original

	if err := harness.attempt.Close(); !errors.Is(err, genuine) ||
		errors.Is(err, context.Canceled) {
		t.Fatalf("immutable graph setup Close=%v", err)
	}
	first := harness.attempt.Outcome()
	firstCauseText := first.Cause().Error()
	firstRetainedText := first.RetainedCause().Error()

	mutateReceiverExposedChildren(t, first.Cause(), &receiverSingleCycleError{})
	mutateReceiverExposedChildren(t, first.RetainedCause(), context.Canceled)
	mutateReceiverExposedChildren(t, original, context.Canceled)
	detachedBenign := first.BenignComponents()
	detachedBenign[0] = ReceiverBenignRemoteOperationMissing
	detachedClasses := first.RetainedCauseClasses()
	detachedClasses[0] = ReceiverCauseRuntimeClosed

	again := harness.attempt.Outcome()
	if again.Cause().Error() != firstCauseText || again.RetainedCause().Error() != firstRetainedText ||
		!errors.Is(again.Cause(), context.Canceled) ||
		!errors.Is(again.Cause(), genuine) ||
		errors.Is(again.RetainedCause(), context.Canceled) ||
		!errors.Is(again.RetainedCause(), genuine) ||
		!errors.Is(again.RetainedCause(), ErrProtocol) ||
		again.RequiresSessionClose() ||
		again.RetainedCause() != harness.attempt.Err() ||
		again.RetainedCause() != harness.attempt.Close() {
		t.Fatalf("published graph changed after mutation: first=%+v again=%+v err=%v", first, again, harness.attempt.Err())
	}
	if containsReceiverCauseClass(again.RetainedCauseClasses(), ReceiverCauseRuntimeClosed) ||
		!containsReceiverBenignCause(again.BenignComponents(), ReceiverBenignContextCanceled) {
		t.Fatalf("published enum slices changed after mutation: %+v", again)
	}
}

func TestReceiverAttemptRemoteDiagnosticSnapshotsAreDetached(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	want := sessionruntime.RemoteOperationFailureSnapshot{}
	remote := sessionruntime.NewRemoteOperationError(want)
	original := errors.Join(remote, ErrProtocol)
	operation.terminateCause = original

	if err := harness.attempt.Close(); err == nil {
		t.Fatal("remote diagnostic setup Close unexpectedly succeeded")
	}
	outcome := harness.attempt.Outcome()
	var first sessionruntime.RemoteOperationError
	if !errors.As(outcome.RetainedCause(), &first) || first.Failure() != want {
		t.Fatalf("first remote diagnostic snapshot=%+v cause=%v", first, outcome.RetainedCause())
	}
	first = sessionruntime.RemoteOperationError{}
	mutateReceiverExposedChildren(t, original, context.Canceled)

	var second sessionruntime.RemoteOperationError
	if !errors.As(harness.attempt.Outcome().RetainedCause(), &second) || second.Failure() != want {
		t.Fatalf("second remote diagnostic snapshot=%+v first=%+v", second, first)
	}
	var fromErr sessionruntime.RemoteOperationError
	if !errors.As(harness.attempt.Err(), &fromErr) || fromErr.Failure() != want {
		t.Fatalf("Err remote diagnostic snapshot=%+v", fromErr)
	}

	const concurrentReaders = 16
	var readers sync.WaitGroup
	readers.Add(concurrentReaders)
	for range concurrentReaders {
		go func() {
			defer readers.Done()
			published := harness.attempt.Outcome()
			mutateReceiverExposedChildren(t, published.Cause(), context.Canceled)
			mutateReceiverExposedChildren(t, published.RetainedCause(), context.Canceled)
			var detached sessionruntime.RemoteOperationError
			if !errors.As(published.RetainedCause(), &detached) {
				t.Errorf("concurrent remote diagnostic missing from %v", published.RetainedCause())
				return
			}
			if detached.Failure() != want {
				t.Errorf("concurrent remote diagnostic=%+v, want %+v", detached.Failure(), want)
			}
		}()
	}
	readers.Wait()
	var final sessionruntime.RemoteOperationError
	if !errors.As(harness.attempt.Close(), &final) || final.Failure() != want {
		t.Fatalf("final remote diagnostic snapshot=%+v", final)
	}
}

func mutateReceiverExposedChildren(t *testing.T, cause, replacement error) {
	t.Helper()
	multi, ok := cause.(interface{ Unwrap() []error })
	if !ok {
		return
	}
	children := multi.Unwrap()
	if len(children) == 0 {
		t.Fatalf("multi-error %T exposed no children", cause)
	}
	children[0] = replacement
	for _, child := range multi.Unwrap() {
		nested, nestedOK := child.(interface{ Unwrap() []error })
		if !nestedOK {
			continue
		}
		nestedChildren := nested.Unwrap()
		if len(nestedChildren) != 0 {
			nestedChildren[0] = replacement
		}
	}
}

func TestReceiverAttemptDoesNotExposeUnsafeTerminalGraph(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	unsafeGraph := &receiverBinaryCycleError{}
	operation.terminateCause = unsafeGraph

	closed := make(chan error, 1)
	go func() { closed <- harness.attempt.Close() }()
	select {
	case err := <-closed:
		if !errors.Is(err, errReceiverOpaqueCause) {
			t.Fatalf("Close residual=%v", err)
		}
	case <-time.After(peerTestTimeout):
		t.Fatal("Close traversed or exposed the cyclic terminal graph")
	}
	outcome := harness.attempt.Outcome()
	if !errors.Is(outcome.Cause(), errReceiverOpaqueCause) ||
		!errors.Is(outcome.RetainedCause(), errReceiverOpaqueCause) ||
		!containsReceiverCauseClass(outcome.RetainedCauseClasses(), ReceiverCauseUnknown) ||
		unsafeGraph.unwrapCalls != 0 {
		t.Fatalf("unsafe terminal outcome=%+v unwrap_calls=%d", outcome, unsafeGraph.unwrapCalls)
	}
}

func TestReceiverAttemptClassifiesStatefulWrapperOnlyOnce(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	stateful := &receiverStatefulWrapperError{}
	operation.terminateCause = stateful

	if err := harness.attempt.Close(); !errors.Is(err, errReceiverOpaqueCause) {
		t.Fatalf("stateful Close residual=%v", err)
	}
	outcome := harness.attempt.Outcome()
	if stateful.unwrapCalls != 0 ||
		!containsReceiverCauseClass(outcome.RetainedCauseClasses(), ReceiverCauseUnknown) {
		t.Fatalf("stateful outcome=%+v unwrap_calls=%d", outcome, stateful.unwrapCalls)
	}
}

func TestReceiverCauseClassTraversalMarksMalformedBranchUnknown(t *testing.T) {
	cause := &receiverJoinedTestError{children: []error{ErrProtocol, nil}}
	classes := ReceiverCauseClasses(cause)
	if len(classes) != 1 || classes[0] != ReceiverCauseUnknown {
		t.Fatalf("malformed multi-error classes=%v", classes)
	}
}

func TestReceiverErrorTreeTraversalBoundsBranchingAndWideGraphs(t *testing.T) {
	binary := &receiverBinaryCycleError{}
	classified := classifyReceiverCause(binary, receiverCausePolicy{})
	if classified.retained != errReceiverOpaqueCause || len(classified.benign) != 0 {
		t.Fatalf("binary cycle residual classification=%+v", classified)
	}
	if binary.unwrapCalls != 0 {
		t.Fatalf("binary cycle residual visited %d nodes", binary.unwrapCalls)
	}
	binary.unwrapCalls = 0
	classes := ReceiverCauseClasses(binary)
	if !containsReceiverCauseClass(classes, ReceiverCauseUnknown) {
		t.Fatalf("binary cycle trace classes=%v", classes)
	}
	if binary.unwrapCalls != 0 {
		t.Fatalf("binary cycle trace visited %d nodes", binary.unwrapCalls)
	}

	uncomparable := receiverUncomparableCycleError{1}
	classified = classifyReceiverCause(uncomparable, receiverCausePolicy{})
	if classified.retained != errReceiverOpaqueCause || len(classified.benign) != 0 {
		t.Fatalf("uncomparable cycle residual classification=%+v", classified)
	}
	classes = ReceiverCauseClasses(uncomparable)
	if !containsReceiverCauseClass(classes, ReceiverCauseUnknown) {
		t.Fatalf("uncomparable cycle trace classes=%v", classes)
	}

	children := make([]error, maximumReceiverErrorTreeNodes+1)
	for index := range children {
		children[index] = ErrProtocol
	}
	wide := errors.Join(children...)
	classified = classifyReceiverCause(wide, receiverCausePolicy{})
	if !errors.Is(classified.retained, errReceiverOpaqueCause) ||
		!errors.Is(classified.retained, ErrProtocol) || len(classified.benign) != 0 {
		t.Fatalf("wide graph residual classification=%+v", classified)
	}
	classes = ReceiverCauseClasses(wide)
	if !containsReceiverCauseClass(classes, ReceiverCauseProtocol) ||
		!containsReceiverCauseClass(classes, ReceiverCauseUnknown) {
		t.Fatalf("wide graph trace classes=%v", classes)
	}
}

func TestReceiverErrorTreeTraversalIsDepthBounded(t *testing.T) {
	deep := error(errors.New("unreachable deep leaf"))
	for depth := range maximumReceiverErrorTreeDepth + 8 {
		deep = fmt.Errorf("trusted receiver depth %d: %w", depth, deep)
	}
	for _, test := range []struct {
		name  string
		cause error
	}{
		{name: "single unwrap cycle", cause: &receiverSingleCycleError{}},
		{name: "multi unwrap cycle", cause: &receiverMultiCycleError{}},
		{name: "beyond maximum depth", cause: deep},
	} {
		t.Run(test.name, func(t *testing.T) {
			classified := classifyReceiverCause(test.cause, receiverCausePolicy{})
			if !errors.Is(classified.retained, errReceiverOpaqueCause) || len(classified.benign) != 0 {
				t.Fatalf("bounded residual classification=%+v", classified)
			}
			classes := ReceiverCauseClasses(test.cause)
			if len(classes) != 1 || classes[0] != ReceiverCauseUnknown {
				t.Fatalf("bounded trace classes=%v", classes)
			}
		})
	}
}
