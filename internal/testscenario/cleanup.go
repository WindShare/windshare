package testscenario

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/windshare/windshare/internal/testrun"
)

const (
	// MaximumCleanupOwners bounds both retained ownership state and the number of
	// caller-controlled callbacks that a terminal transition can attempt.
	MaximumCleanupOwners = 32
	// MaximumCleanupOwnerNameBytes bounds retained labels and failure diagnostics.
	MaximumCleanupOwnerNameBytes = 128
	// CleanupOwnerTimeout bounds one uncooperative owner, while
	// ScenarioCleanupTimeout bounds the complete reverse-order cleanup transition.
	CleanupOwnerTimeout    = 15 * time.Second
	ScenarioCleanupTimeout = 60 * time.Second
)

var (
	// ErrCleanupTimeout identifies an owner that did not honor its cleanup lease.
	ErrCleanupTimeout = errors.New("test scenario cleanup timed out")
	// ErrCleanupPanic isolates a caller cleanup panic from the terminal owner.
	ErrCleanupPanic = errors.New("test scenario cleanup panicked")
)

type cleanupOwner struct {
	name    string
	cleanup CleanupFunc
}

// CleanupFunc receives a framework-owned lease. The lifecycle also enforces the
// lease mechanically, so a callback that ignores cancellation cannot block the
// remaining cleanup owners or terminal evidence.
type CleanupFunc func(context.Context) error

// CleanupContext makes aggregate cleanup failure count machine-readable.
type CleanupContext struct {
	FailureCount int `json:"failure_count"`
}

// AddCleanup transfers cleanup verdict ownership to the scenario.
func (trace *Trace) AddCleanup(name string, cleanup CleanupFunc) error {
	if trace == nil || validateCleanupOwnerName(name) != nil || cleanup == nil {
		return ErrInvalid
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.lifecycle != lifecycleActive {
		return ErrRetired
	}
	if len(trace.cleanupOwners) >= MaximumCleanupOwners {
		return ErrCapacity
	}
	trace.cleanupOwners = append(trace.cleanupOwners, cleanupOwner{name: name, cleanup: cleanup})
	return nil
}

// RequireCleanup binds AddCleanup failure to the calling test verdict.
func (trace *Trace) RequireCleanup(t testContext, name string, cleanup CleanupFunc) {
	t.Helper()
	if err := trace.AddCleanup(name, cleanup); err != nil {
		t.Fatalf("register %s cleanup: %v", name, err)
	}
}

func (trace *Trace) finishCleanup(owners []cleanupOwner) (testrun.Outcome, error) {
	trace.recordFinal(testrun.CleanupMilestone, testrun.OutcomeStarted, nil)
	var failures []error
	cleanupContext, cancelCleanup := context.WithTimeout(
		context.Background(),
		trace.scenarioCleanupTimeout,
	)
	defer cancelCleanup()
	for index := range slices.Backward(owners) {
		owner := owners[index]
		if err := trace.runCleanupOwner(cleanupContext, owner, index+1); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", owner.name, err))
		}
	}
	cleanupErr := errors.Join(failures...)
	cleanupOutcome := outcomeFor(cleanupErr == nil)
	trace.recordFinal(
		testrun.CleanupMilestone,
		cleanupOutcome,
		CleanupContext{FailureCount: len(failures)},
	)
	return cleanupOutcome, cleanupErr
}

func (trace *Trace) runCleanupOwner(
	parent context.Context,
	owner cleanupOwner,
	remainingOwnerCount int,
) error {
	ownerLease := trace.cleanupOwnerTimeout
	if deadline, bounded := parent.Deadline(); bounded && remainingOwnerCount > 0 {
		// A fair share keeps one wedged reverse-order owner from consuming the
		// complete scenario budget before the remaining owners are attempted.
		fairShare := time.Until(deadline) / time.Duration(remainingOwnerCount)
		if fairShare < ownerLease {
			ownerLease = fairShare
		}
	}
	if ownerLease <= 0 {
		ownerLease = time.Nanosecond
	}
	ownerContext, cancelOwner := context.WithTimeout(parent, ownerLease)
	defer cancelOwner()
	result := make(chan error, 1)
	go func() {
		result <- invokeCleanup(owner.cleanup, ownerContext)
	}()
	select {
	case err := <-result:
		return err
	case <-ownerContext.Done():
		return fmt.Errorf("%w: %w", ErrCleanupTimeout, ownerContext.Err())
	}
}

func invokeCleanup(cleanup CleanupFunc, cleanupContext context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("cleanup panic type %T: %w", recovered, ErrCleanupPanic)
		}
	}()
	cleanupErr := cleanup(cleanupContext)
	if cleanupErr == nil {
		return nil
	}
	return &cleanupHookFailure{cause: cleanupErr}
}

type cleanupHookFailure struct {
	cause error
}

func (*cleanupHookFailure) Error() string { return "test scenario cleanup callback returned an error" }

func (failure *cleanupHookFailure) Unwrap() error { return failure.cause }

func validateCleanupOwnerName(name string) error {
	if len(name) == 0 || len(name) > MaximumCleanupOwnerNameBytes || name[0] == ' ' || name[len(name)-1] == ' ' {
		return ErrInvalid
	}
	for index := range len(name) {
		character := name[index]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == ' ' || character == '_' ||
			character == '-' || character == '.' {
			continue
		}
		return ErrInvalid
	}
	return nil
}
