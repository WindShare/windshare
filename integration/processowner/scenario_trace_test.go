//go:build windows || linux

package processowner_test

import (
	"os"
	"sync"
	"testing"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testscenario"
)

const processOwnerIntegrationComponent testrun.Component = "integration_processowner"

var processOwnerTraceOutput struct {
	sync.Once
	sink *testrun.JSONLineSink
	err  error
}

func startProcessOwnerScenario(t *testing.T, scenarioName string) (*testscenario.Trace, protocol.Identity) {
	t.Helper()
	run, err := testrun.PackageRun()
	if err != nil {
		t.Fatalf("resolve process-owner integration run: %v", err)
	}
	operation, err := run.NewOperation(scenarioName)
	if err != nil {
		t.Fatalf("create process-owner integration operation: %v", err)
	}
	sink, err := processOwnerIntegrationSink()
	if err != nil {
		t.Fatalf("create process-owner integration event sink: %v", err)
	}
	trace := testscenario.Start(t, operation, processOwnerIntegrationComponent, sink)
	return trace, operation.EventIdentity()
}

func finishProcessOwnerScenario(t *testing.T, trace *testscenario.Trace) {
	t.Helper()
	trace.RequireSuccess(t)
}

func processOwnerIntegrationSink() (*testrun.JSONLineSink, error) {
	processOwnerTraceOutput.Do(func() {
		processOwnerTraceOutput.sink, processOwnerTraceOutput.err = testrun.NewJSONLineSink(os.Stdout)
	})
	return processOwnerTraceOutput.sink, processOwnerTraceOutput.err
}
