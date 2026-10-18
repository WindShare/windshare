package relayv2_test

import (
	"os"
	"sync"
	"testing"

	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testscenario"
)

const (
	relayWebSocketScenario = "integration/relayv2/real-websocket-frame-exchange"

	relayIntegrationComponent testrun.Component = "integration_relayv2"
	frameExchangeMilestone    testrun.Milestone = "frame_exchange"
)

var relayTraceOutput struct {
	sync.Once
	sink *testrun.JSONLineSink
	err  error
}

func startRelayScenario(t *testing.T, scenario string) (*testscenario.Trace, testrun.Identity) {
	t.Helper()
	run, err := testrun.PackageRun()
	if err != nil {
		t.Fatalf("resolve relay integration run: %v", err)
	}
	operation, err := run.NewOperation(scenario)
	if err != nil {
		t.Fatalf("create relay integration operation: %v", err)
	}
	sink, err := relayIntegrationSink()
	if err != nil {
		t.Fatalf("create relay integration event sink: %v", err)
	}
	return testscenario.Start(t, operation, relayIntegrationComponent, sink), testrun.Identity{
		RunID: operation.RunID(), OperationID: operation.ID(), Scenario: operation.Scenario(),
	}
}

func relayIntegrationSink() (*testrun.JSONLineSink, error) {
	relayTraceOutput.Do(func() {
		relayTraceOutput.sink, relayTraceOutput.err = testrun.NewJSONLineSink(os.Stdout)
	})
	return relayTraceOutput.sink, relayTraceOutput.err
}
