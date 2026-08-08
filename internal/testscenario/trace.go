// Package testscenario owns lifecycle evidence and cleanup verdicts for native
// integration and process E2E scenarios. Protocol-specific tests retain their
// own milestones; this package owns the ordering rules every scenario shares.
package testscenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/windshare/windshare/internal/testrun"
)

const (
	// MaximumFailureReasonBytes keeps machine-consumed failure decisions bounded.
	MaximumFailureReasonBytes = 128
	// MaximumPhasesPerScenario prevents a faulty fixture from retaining an
	// unbounded number of settlement authorities.
	MaximumPhasesPerScenario = 128
	// MaximumScenarioEvidenceEvents reserves enough recorder capacity for every
	// phase settlement plus the cleanup and terminal records owned by Finish.
	MaximumScenarioEvidenceEvents = 512
	// InterruptedFailureReason closes a phase whose caller never published a result.
	InterruptedFailureReason = "scenario_interrupted"

	terminalEventReserve  = 3
	lifecycleStartReserve = 1
)

var (
	// ErrInvalid rejects malformed ownership or lifecycle operations.
	ErrInvalid = errors.New("test scenario lifecycle is invalid")
	// ErrRetired prevents mutation after the functional seal or terminal owner takes control.
	ErrRetired = errors.New("test scenario lifecycle is retired")
	// ErrIncomplete means the functional oracle was never authoritatively marked successful.
	ErrIncomplete = errors.New("test scenario did not reach its verified success boundary")
	// ErrPhaseActive prevents success while a started milestone lacks an outcome.
	ErrPhaseActive = errors.New("test scenario phase is still active")
	// ErrPhaseSettled prevents a second, contradictory outcome for one started milestone.
	ErrPhaseSettled = errors.New("test scenario phase is already settled")
	// ErrFailureReason rejects unbounded or non-semantic failure prose.
	ErrFailureReason = errors.New("test scenario failure reason is invalid")
	// ErrCapacity rejects lifecycle growth that would make terminal evidence or
	// callback latency unbounded.
	ErrCapacity = errors.New("test scenario lifecycle capacity exceeded")
)

type testContext interface {
	Helper()
	Fatalf(string, ...any)
}

type lifecycleTestContext interface {
	testContext
	Cleanup(func())
	Errorf(string, ...any)
}

type lifecycle uint8

const (
	lifecycleActive lifecycle = iota + 1
	lifecycleVerified
	lifecycleFinishing
	lifecycleFinished
)

// Trace is the single verdict owner for a scenario. Its mutex linearizes event
// publication against Finish so no milestone can appear after the terminal event.
type Trace struct {
	recorder *testrun.Recorder

	mu                     sync.Mutex
	lifecycle              lifecycle
	cleanupOwners          []cleanupOwner
	phases                 []*Phase
	functionalVerified     bool
	lastMilestone          testrun.Milestone
	authoritativeError     error
	deliveryError          error
	evidenceSlots          int
	cleanupOwnerTimeout    time.Duration
	scenarioCleanupTimeout time.Duration

	finishOnce sync.Once
	finishErr  error
}

// Phase pairs one started milestone with exactly one success or bounded semantic
// failure. It deliberately carries no protocol data beyond the milestone itself.
type Phase struct {
	trace     *Trace
	milestone testrun.Milestone
	settled   bool
	succeeded bool
}

// FailureContext carries a stable decision token rather than unbounded error prose.
type FailureContext struct {
	Reason string `json:"reason"`
}

// TerminalContext separates functional and cleanup verdicts for diagnosis.
type TerminalContext struct {
	FunctionalOutcome    testrun.Outcome   `json:"functional_outcome"`
	CleanupOutcome       testrun.Outcome   `json:"cleanup_outcome"`
	PriorEvidenceOutcome testrun.Outcome   `json:"prior_evidence_outcome"`
	PriorDeliveryOutcome testrun.Outcome   `json:"prior_delivery_outcome"`
	LastMilestone        testrun.Milestone `json:"last_milestone"`
}

// New starts one correlated scenario and immediately publishes its started event.
func New(
	operation testrun.Operation,
	component testrun.Component,
	sink testrun.EventSink,
) (*Trace, error) {
	return newTrace(
		operation,
		component,
		sink,
		CleanupOwnerTimeout,
		ScenarioCleanupTimeout,
	)
}

func newTrace(
	operation testrun.Operation,
	component testrun.Component,
	sink testrun.EventSink,
	cleanupOwnerTimeout time.Duration,
	scenarioCleanupTimeout time.Duration,
) (*Trace, error) {
	if cleanupOwnerTimeout <= 0 || scenarioCleanupTimeout <= 0 ||
		MaximumScenarioEvidenceEvents+terminalEventReserve+lifecycleStartReserve >
			testrun.MaximumRecordedEvents {
		return nil, ErrInvalid
	}
	recorder, err := testrun.NewRecorder(operation, component, sink)
	if err != nil {
		return nil, err
	}
	trace := &Trace{
		recorder:               recorder,
		lifecycle:              lifecycleActive,
		lastMilestone:          testrun.ScenarioLifecycleMilestone,
		cleanupOwnerTimeout:    cleanupOwnerTimeout,
		scenarioCleanupTimeout: scenarioCleanupTimeout,
	}
	trace.mu.Lock()
	err = trace.recordFrameworkLocked(testrun.ScenarioLifecycleMilestone, testrun.OutcomeStarted, nil)
	trace.mu.Unlock()
	if err != nil {
		return trace, err
	}
	return trace, nil
}

// Start installs terminal ownership before surfacing a started-event error.
// A sink may persist an event and then fail, so callers must never have to
// remember this ordering at each integration or E2E entry point.
func Start(
	t lifecycleTestContext,
	operation testrun.Operation,
	component testrun.Component,
	sink testrun.EventSink,
) *Trace {
	t.Helper()
	trace, err := New(operation, component, sink)
	if trace != nil {
		t.Cleanup(func() {
			if trace.Finished() {
				return
			}
			if finishErr := trace.Finish(); finishErr != nil {
				t.Errorf("finish test scenario: %v", finishErr)
			}
		})
	}
	if err != nil {
		t.Fatalf("start test scenario: %v", err)
	}
	if trace == nil {
		t.Fatalf("start test scenario returned no lifecycle owner")
	}
	return trace
}

// Record emits an already-decided protocol-specific point observation. Started
// outcomes require StartPhase so the lifecycle owner can enforce one settlement.
// Lifecycle and cleanup milestones remain reserved for this owner.
func (trace *Trace) Record(
	milestone testrun.Milestone,
	outcome testrun.Outcome,
	payload any,
) error {
	if trace == nil || trace.recorder == nil {
		return ErrInvalid
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.lifecycle != lifecycleActive {
		return ErrRetired
	}
	if reservedMilestone(milestone) {
		return ErrInvalid
	}
	if err := testrun.ValidateMilestone(milestone); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if outcome != testrun.OutcomeSucceeded && outcome != testrun.OutcomeFailed {
		return ErrInvalid
	}
	if trace.evidenceSlots >= MaximumScenarioEvidenceEvents {
		return ErrCapacity
	}
	prepared, err := trace.recorder.PrepareEvent(milestone, outcome, payload)
	if err != nil {
		trace.rememberRecordErrorLocked(err)
		return err
	}
	trace.evidenceSlots++
	trace.lastMilestone = milestone
	return trace.recordPreparedLocked(prepared)
}

// RequireRecord binds Record failure to the calling test verdict.
func (trace *Trace) RequireRecord(
	t testContext,
	milestone testrun.Milestone,
	outcome testrun.Outcome,
	payload any,
) {
	t.Helper()
	if err := trace.Record(milestone, outcome, payload); err != nil {
		t.Fatalf("record scenario milestone %s/%s: %v", milestone, outcome, err)
	}
}

// StartPhase publishes started and returns its sole settlement authority. The
// phase remains registered when a sink returns an error because the write may
// already be externally visible; Finish will then publish a matching failure.
func (trace *Trace) StartPhase(milestone testrun.Milestone, payload any) (*Phase, error) {
	if trace == nil || trace.recorder == nil || reservedMilestone(milestone) {
		return nil, ErrInvalid
	}
	if err := testrun.ValidateMilestone(milestone); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.lifecycle != lifecycleActive {
		return nil, ErrRetired
	}
	if len(trace.phases) >= MaximumPhasesPerScenario ||
		trace.evidenceSlots+2 > MaximumScenarioEvidenceEvents {
		return nil, ErrCapacity
	}
	prepared, err := trace.recorder.PrepareEvent(milestone, testrun.OutcomeStarted, payload)
	if err != nil {
		trace.rememberRecordErrorLocked(err)
		return nil, err
	}
	phase := &Phase{trace: trace, milestone: milestone}
	trace.phases = append(trace.phases, phase)
	// Reserve the settlement slot now so Finish can always close an interrupted
	// phase without competing with later point observations.
	trace.evidenceSlots += 2
	trace.lastMilestone = milestone
	return phase, trace.recordPreparedLocked(prepared)
}

// Succeed publishes the phase's only successful settlement.
func (phase *Phase) Succeed(payload any) error {
	return phase.settle(testrun.OutcomeSucceeded, payload)
}

// Fail publishes the phase's only failed settlement with a bounded reason.
func (phase *Phase) Fail(reason string) error {
	if err := validateFailureReason(reason); err != nil {
		return err
	}
	return phase.settle(testrun.OutcomeFailed, FailureContext{Reason: reason})
}

func (phase *Phase) settle(outcome testrun.Outcome, payload any) error {
	if phase == nil || phase.trace == nil {
		return ErrInvalid
	}
	trace := phase.trace
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.lifecycle != lifecycleActive {
		return ErrRetired
	}
	if phase.settled {
		return ErrPhaseSettled
	}
	prepared, err := trace.recorder.PrepareEvent(phase.milestone, outcome, payload)
	if err != nil {
		trace.rememberRecordErrorLocked(err)
		return err
	}
	// Settlement remains final even when the sink fails. Retrying could emit two
	// contradictory outcomes if the first write reached an external consumer.
	phase.settled = true
	phase.succeeded = outcome == testrun.OutcomeSucceeded
	trace.lastMilestone = phase.milestone
	return trace.recordPreparedLocked(prepared)
}

// MarkFunctionalSuccess seals the functional oracle only after every phase succeeds.
// A successful seal rejects all later evidence, cleanup registration, and phase
// settlement so Finish observes the exact state that was authoritatively verified.
func (trace *Trace) MarkFunctionalSuccess() error {
	if trace == nil {
		return ErrInvalid
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.lifecycle != lifecycleActive {
		return ErrRetired
	}
	if trace.functionalVerified {
		return ErrInvalid
	}
	if trace.authoritativeError != nil {
		return ErrIncomplete
	}
	for _, phase := range trace.phases {
		if !phase.settled {
			return ErrPhaseActive
		}
		if !phase.succeeded {
			return ErrIncomplete
		}
	}
	trace.functionalVerified = true
	trace.lifecycle = lifecycleVerified
	return nil
}

// RequireSuccess seals the functional oracle, runs every cleanup owner, and
// makes functional, cleanup, and evidence-publication failures one test verdict.
func (trace *Trace) RequireSuccess(t testContext) {
	t.Helper()
	markErr := trace.MarkFunctionalSuccess()
	finishErr := trace.Finish()
	if err := errors.Join(markErr, finishErr); err != nil {
		t.Fatalf("finish successful test scenario: %v", err)
	}
}

// Finish retires cleanup owners in reverse order and publishes one terminal verdict.
func (trace *Trace) Finish() error {
	if trace == nil || trace.recorder == nil {
		return ErrInvalid
	}
	trace.finishOnce.Do(func() {
		trace.finishErr = trace.finish()
	})
	return trace.finishErr
}

// Finished reports whether the terminal boundary has been published or attempted.
func (trace *Trace) Finished() bool {
	if trace == nil {
		return true
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return trace.lifecycle == lifecycleFinished
}

// Events returns the authoritative bounded journal. External delivery is best
// effort and separately reported; lifecycle correctness is decided from this
// pull-only snapshot.
func (trace *Trace) Events() []testrun.Event {
	if trace == nil || trace.recorder == nil {
		return nil
	}
	return trace.recorder.Events()
}

func (trace *Trace) finish() error {
	trace.mu.Lock()
	if trace.lifecycle != lifecycleActive && trace.lifecycle != lifecycleVerified {
		trace.mu.Unlock()
		return ErrRetired
	}
	trace.lifecycle = lifecycleFinishing
	owners := append([]cleanupOwner(nil), trace.cleanupOwners...)
	lastMilestone := trace.lastMilestone
	// Verification is necessary but not sufficient: re-derive the phase invariant
	// so a stale or internally inconsistent seal can never create a success verdict.
	functionalSucceeded := trace.functionalVerified
	openPhases := make([]*Phase, 0)
	for _, phase := range trace.phases {
		if phase.settled && phase.succeeded {
			continue
		}
		functionalSucceeded = false
		if !phase.settled {
			phase.settled = true
			openPhases = append(openPhases, phase)
		}
	}
	trace.mu.Unlock()

	for _, phase := range openPhases {
		trace.recordFinal(
			phase.milestone,
			testrun.OutcomeFailed,
			FailureContext{Reason: InterruptedFailureReason},
		)
	}
	cleanupOutcome, cleanupErr := trace.finishCleanup(owners)

	priorEvidenceErr := trace.joinAuthoritativeErrors()
	priorDeliveryErr := trace.joinDeliveryErrors()
	priorEvidenceOutcome := outcomeFor(priorEvidenceErr == nil)
	priorDeliveryOutcome := outcomeFor(priorDeliveryErr == nil)
	terminalOutcome := outcomeFor(
		functionalSucceeded && len(openPhases) == 0 && cleanupErr == nil &&
			priorEvidenceErr == nil && priorDeliveryErr == nil,
	)
	trace.recordFinal(
		testrun.ScenarioLifecycleMilestone,
		terminalOutcome,
		TerminalContext{
			FunctionalOutcome:    outcomeFor(functionalSucceeded),
			CleanupOutcome:       cleanupOutcome,
			PriorEvidenceOutcome: priorEvidenceOutcome,
			PriorDeliveryOutcome: priorDeliveryOutcome,
			LastMilestone:        lastMilestone,
		},
	)
	recordErr := trace.joinRecordErrors()

	trace.mu.Lock()
	trace.lifecycle = lifecycleFinished
	trace.mu.Unlock()

	var functionalErr error
	if !functionalSucceeded {
		functionalErr = ErrIncomplete
	}
	var phaseErr error
	if len(openPhases) != 0 {
		phaseErr = fmt.Errorf("%w: %d", ErrPhaseActive, len(openPhases))
	}
	return errors.Join(functionalErr, phaseErr, cleanupErr, recordErr)
}

func (trace *Trace) recordFinal(
	milestone testrun.Milestone,
	outcome testrun.Outcome,
	payload any,
) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	_ = trace.recordFrameworkLocked(milestone, outcome, payload)
}

func (trace *Trace) recordPreparedLocked(prepared testrun.PreparedEvent) error {
	err := trace.recorder.RecordPrepared(prepared)
	if err != nil {
		trace.rememberRecordErrorLocked(err)
	}
	return err
}

func (trace *Trace) recordFrameworkLocked(
	milestone testrun.Milestone,
	outcome testrun.Outcome,
	payload any,
) error {
	encodedPayload, err := encodeFrameworkPayload(payload)
	if err == nil {
		err = trace.recorder.RecordEncoded(milestone, outcome, encodedPayload)
	}
	if err != nil {
		trace.rememberRecordErrorLocked(err)
	}
	return err
}

func encodeFrameworkPayload(payload any) (json.RawMessage, error) {
	switch payload.(type) {
	case nil, FailureContext, CleanupContext, TerminalContext:
	default:
		return nil, ErrInvalid
	}
	if payload == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode closed scenario payload: %w", err)
	}
	return encoded, nil
}

func (trace *Trace) rememberRecordErrorLocked(err error) {
	if err == nil {
		return
	}
	if testrun.IsDeliveryFailure(err) {
		if trace.deliveryError == nil {
			trace.deliveryError = err
		}
	} else if trace.authoritativeError == nil {
		trace.authoritativeError = err
	}
}

func (trace *Trace) joinRecordErrors() error {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return errors.Join(trace.authoritativeError, trace.deliveryError)
}

func (trace *Trace) joinAuthoritativeErrors() error {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return trace.authoritativeError
}

func (trace *Trace) joinDeliveryErrors() error {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return trace.deliveryError
}

func validateFailureReason(reason string) error {
	if len(reason) == 0 || len(reason) > MaximumFailureReasonBytes {
		return ErrFailureReason
	}
	for index := range len(reason) {
		character := reason[index]
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' {
			continue
		}
		return ErrFailureReason
	}
	return nil
}

func reservedMilestone(milestone testrun.Milestone) bool {
	return milestone == testrun.ScenarioLifecycleMilestone || milestone == testrun.CleanupMilestone
}

func outcomeFor(succeeded bool) testrun.Outcome {
	if succeeded {
		return testrun.OutcomeSucceeded
	}
	return testrun.OutcomeFailed
}
