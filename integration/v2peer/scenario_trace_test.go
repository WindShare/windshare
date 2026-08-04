package v2peer_test

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testscenario"
)

const (
	senderRealPionScenario   = "integration/v2peer/sender-real-pion"
	receiverRealPionScenario = "integration/v2peer/receiver-sender-real-pion"

	senderRealPionComponent   testrun.Component = "integration_v2peer_sender"
	receiverRealPionComponent testrun.Component = "integration_v2peer_receiver"

	peerReadinessEvidenceStartFailureReason = "peer_ready_evidence_start_failed"
	laneAdoptionEvidenceStartFailureReason  = "lane_adoption_evidence_start_failed"
)

type integrationLaneContext struct {
	LaneID    uint32 `json:"lane_id"`
	LaneEpoch uint32 `json:"lane_epoch"`
}

var integrationTraceOutput struct {
	sync.Once
	sink *testrun.JSONLineSink
	err  error
}

func requireLongPionIntegration(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("real Pion network integration is owned by the long integration gate")
	}
}

func startIntegrationScenario(
	t *testing.T,
	scenarioName string,
	component testrun.Component,
) *testscenario.Trace {
	t.Helper()
	run, err := testrun.PackageRun()
	if err != nil {
		t.Fatalf("resolve integration test run: %v", err)
	}
	operation, err := run.NewOperation(scenarioName)
	if err != nil {
		t.Fatalf("create integration trace operation: %v", err)
	}
	sink, err := integrationJSONLineSink()
	if err != nil {
		t.Fatalf("create integration trace sink: %v", err)
	}
	return testscenario.Start(t, operation, component, sink)
}

func startIntegrationPhase(
	t *testing.T,
	trace *testscenario.Trace,
	milestone testrun.Milestone,
	payload any,
	startFailureReason string,
) *testscenario.Phase {
	t.Helper()
	phase, err := trace.StartPhase(milestone, payload)
	if err == nil {
		return phase
	}
	if phase != nil {
		err = errors.Join(err, phase.Fail(startFailureReason))
	}
	t.Fatalf("start integration phase %s: %v", milestone, err)
	return nil
}

func failIntegrationPhase(
	t *testing.T,
	phase *testscenario.Phase,
	reason string,
	format string,
	arguments ...any,
) {
	t.Helper()
	message := fmt.Sprintf(format, arguments...)
	if err := phase.Fail(reason); err != nil {
		t.Fatalf("%s; record integration phase failure: %v", message, err)
	}
	t.Fatal(message)
}

func integrationJSONLineSink() (*testrun.JSONLineSink, error) {
	integrationTraceOutput.Do(func() {
		integrationTraceOutput.sink, integrationTraceOutput.err = testrun.NewJSONLineSink(os.Stdout)
	})
	return integrationTraceOutput.sink, integrationTraceOutput.err
}

func joinIntegrationRoutine(
	name string,
	done <-chan error,
	timeout time.Duration,
	accept func(error) bool,
) error {
	if name == "" || done == nil || timeout <= 0 || accept == nil {
		return errors.New("integration routine join contract is invalid")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if !accept(err) {
			return fmt.Errorf("%s returned unexpected terminal error: %v", name, err)
		}
		return nil
	case <-timer.C:
		return fmt.Errorf("%s did not stop within %s", name, timeout)
	}
}

func joinIntegrationSignal(
	name string,
	done <-chan struct{},
	timeout time.Duration,
	result func() error,
) error {
	if name == "" || done == nil || timeout <= 0 || result == nil {
		return errors.New("integration signal join contract is invalid")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return result()
	case <-timer.C:
		return fmt.Errorf("%s did not stop within %s", name, timeout)
	}
}
