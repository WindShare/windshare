package e2e

import (
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testscenario"
)

const (
	v2E2EComponent testrun.Component = "e2e_go_process"

	v2ProcessStartMilestone       testrun.Milestone = "process_start"
	v2ProcessReadinessMilestone   testrun.Milestone = "process_readiness"
	v2ProcessWaitMilestone        testrun.Milestone = "process_wait"
	v2ProcessStopMilestone        testrun.Milestone = "process_stop"
	v2ProcessEventWaitMilestone   testrun.Milestone = "process_event_wait"
	v2ProcessEventDrainMilestone  testrun.Milestone = "process_event_drain"
	v2ArtifactCheckpointMilestone testrun.Milestone = "artifact_checkpoint_wait"
	v2RelayProxyForwardMilestone  testrun.Milestone = "relay_proxy_forward"
	v2RelayProxyPauseMilestone    testrun.Milestone = "relay_proxy_pause"
	v2RelayProxyResumeMilestone   testrun.Milestone = "relay_proxy_resume"
	v2RelayProxyCutMilestone      testrun.Milestone = "relay_proxy_cut"
	v2ScenarioOracleMilestone     testrun.Milestone = "scenario_oracle"

	v2EvidenceStartFailureReason = "evidence_start_failed"
	v2ActionFailureReason        = "scenario_action_failed"
	v2ProcessWaitFailureReason   = "process_wait_failed"
)

type v2Scenario struct {
	operation testrun.Operation
	trace     *testscenario.Trace
}

type v2ProcessPhaseContext struct {
	Component string `json:"component"`
}

var v2ScenarioOutput struct {
	sync.Once
	sink *testrun.JSONLineSink
	err  error
}

func startV2Scenario(t *testing.T, name string) *v2Scenario {
	t.Helper()
	run, err := testrun.PackageRun()
	if err != nil {
		t.Fatalf("resolve E2E run identity: %v", err)
	}
	operation, err := run.NewOperation(name)
	if err != nil {
		t.Fatalf("create %s operation: %v", name, err)
	}
	sink, err := v2ScenarioSink()
	if err != nil {
		t.Fatalf("create E2E scenario event sink: %v", err)
	}
	return &v2Scenario{
		operation: operation,
		trace:     testscenario.Start(t, operation, v2E2EComponent, sink),
	}
}

func v2ScenarioSink() (*testrun.JSONLineSink, error) {
	v2ScenarioOutput.Do(func() {
		v2ScenarioOutput.sink, v2ScenarioOutput.err = testrun.NewJSONLineSink(os.Stdout)
	})
	return v2ScenarioOutput.sink, v2ScenarioOutput.err
}

func (scenario *v2Scenario) startPhase(
	t *testing.T,
	milestone testrun.Milestone,
	payload any,
) *testscenario.Phase {
	t.Helper()
	if scenario == nil || scenario.trace == nil {
		t.Fatal("E2E scenario lifecycle owner is nil")
	}
	phase, err := scenario.trace.StartPhase(milestone, payload)
	if err == nil {
		return phase
	}
	// The started event may already be durable when its sink reports an error.
	// Settling the retained authority prevents an orphaned phase before Fatalf.
	if phase != nil {
		err = errors.Join(err, phase.Fail(v2EvidenceStartFailureReason))
	}
	t.Fatalf("start E2E scenario phase %s: %v", milestone, err)
	return nil
}

func (scenario *v2Scenario) succeedPhase(t *testing.T, phase *testscenario.Phase, payload any) {
	t.Helper()
	if err := phase.Succeed(payload); err != nil {
		t.Fatalf("complete E2E scenario phase: %v", err)
	}
}

func (scenario *v2Scenario) observe(
	milestone testrun.Milestone,
	payload any,
	action func() error,
) error {
	if scenario == nil || scenario.trace == nil || action == nil {
		return testscenario.ErrInvalid
	}
	phase, err := scenario.trace.StartPhase(milestone, payload)
	if err != nil {
		if phase != nil {
			err = errors.Join(err, phase.Fail(v2EvidenceStartFailureReason))
		}
		return err
	}
	if err := action(); err != nil {
		return errors.Join(err, phase.Fail(v2ActionFailureReason))
	}
	return phase.Succeed(payload)
}

func (scenario *v2Scenario) requireSuccess(t *testing.T) {
	t.Helper()
	scenario.trace.RequireRecord(
		t,
		v2ScenarioOracleMilestone,
		testrun.OutcomeSucceeded,
		nil,
	)
	scenario.trace.RequireSuccess(t)
}
