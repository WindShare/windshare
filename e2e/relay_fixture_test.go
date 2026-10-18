package e2e

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/internal/testprocess"
	"github.com/windshare/windshare/internal/testrelaynative"
	"github.com/windshare/windshare/internal/testrun"
)

const (
	v2RelayComponent         = "relay_native"
	v2RelayProxyEndpointPath = "/e2e/v2/ws"
)

type v2RelayFixture struct {
	scenario *v2Scenario
	native   *testrelaynative.Fixture
}

type v2RelayProcess struct {
	scenario *v2Scenario
	host     *testrelaynative.Host
	owner    *testprocess.Owner
	stdout   *processOutputView
	stderr   *processOutputView
	address  string
	baseURL  string
}

type v2RelayReadyContext struct {
	Component string `json:"component"`
	Database  string `json:"database"`
	Address   string `json:"address"`
	Endpoint  string `json:"endpoint"`
}

func newV2RelayFixture(t *testing.T, scenario *v2Scenario, publicEndpoint string) *v2RelayFixture {
	t.Helper()
	return &v2RelayFixture{
		scenario: scenario,
		native:   testrelaynative.NewFixture(t.TempDir(), publicEndpoint),
	}
}

func startV2Relay(t *testing.T, scenario *v2Scenario) *v2RelayProcess {
	t.Helper()
	return newV2RelayFixture(t, scenario, "").start(t)
}

func (fixture *v2RelayFixture) start(t *testing.T) *v2RelayProcess {
	t.Helper()
	binaries := loadE2EBinaries(t)
	owner, err := testprocess.NewOwner(binaries.processOwner)
	if err != nil {
		t.Fatalf("open native relay process owner: %v", err)
	}
	process := &v2RelayProcess{scenario: fixture.scenario, owner: owner}
	// TempDir cleanup precedes scenario finalization. Retire the process first
	// so Windows can remove SQLite, even when startup or the scenario fails.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), v2ProcessTerminationGrace)
		defer cancel()
		if err := process.close(ctx); err != nil {
			t.Errorf("clean native relay: %v", err)
		}
	})
	fixture.scenario.trace.RequireCleanup(t, "relay_native process tree", process.close)
	phase := fixture.scenario.startPhase(t, v2ProcessReadinessMilestone, v2ProcessPhaseContext{Component: v2RelayComponent})
	operation := fixture.scenario.operation
	process.host, err = fixture.native.Start(t.Context(), owner, testrelaynative.Launch{
		Executable: binaries.relay, WorkingDirectory: repoRoot(),
		Identity: testrun.Identity{
			RunID: operation.RunID(), OperationID: operation.ID(), Scenario: operation.Scenario(),
		},
	})
	if err != nil {
		t.Fatalf("start native relay: %v", err)
	}
	process.stdout = &processOutputView{snapshot: process.host.Stdout}
	process.stderr = &processOutputView{snapshot: process.host.Stderr}
	ready := process.host.Ready
	process.address, process.baseURL = ready.Address, ready.BaseURL
	fixture.scenario.succeedPhase(t, phase, v2RelayReadyContext{
		Component: v2RelayComponent, Database: fixture.native.Database(),
		Address: ready.Address, Endpoint: ready.Endpoint,
	})
	return process
}

func (process *v2RelayProcess) close(ctx context.Context) error {
	var stopErr error
	if process.host != nil {
		stopErr = process.host.Close(ctx)
	}
	return errors.Join(stopErr, process.owner.Close())
}

func (process *v2RelayProcess) stop(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), v2ProcessTerminationGrace)
	defer cancel()
	if err := process.scenario.observe(v2ProcessStopMilestone, v2ProcessPhaseContext{Component: v2RelayComponent}, func() error {
		return process.close(ctx)
	}); err != nil {
		t.Fatalf("stop native relay: %v", err)
	}
}
