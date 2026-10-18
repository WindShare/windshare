package testrelaynative

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/windshare/windshare/internal/testprocess"
	"github.com/windshare/windshare/internal/testrun"
)

const (
	loopbackAddress  = "127.0.0.1:0"
	databaseFilename = "relay.sqlite"
	readinessTimeout = 30 * time.Second
	processDeadline  = 5 * time.Minute
	terminationGrace = 10 * time.Second
)

type processStarter interface {
	Start(context.Context, testprocess.Spec) (*testprocess.Process, error)
}

type Launch struct {
	Executable       string
	WorkingDirectory string
	Identity         testrun.Identity
}

// Fixture retains durable state and endpoint identity across sequential starts.
// Each scenario supplies its own temporary directory; the process owner is shared
// independently so preparing another relay never rebuilds its supervisor.
type Fixture struct {
	database       string
	listenAddress  string
	publicEndpoint string
}

func NewFixture(stateDirectory, publicEndpoint string) *Fixture {
	return &Fixture{
		database:      filepath.Join(stateDirectory, databaseFilename),
		listenAddress: loopbackAddress, publicEndpoint: publicEndpoint,
	}
}

func (fixture *Fixture) Database() string { return fixture.database }

type Host struct {
	Ready   Readiness
	process *testprocess.Process
}

func (fixture *Fixture) Start(ctx context.Context, owner processStarter, launch Launch) (*Host, error) {
	environment, err := testprocess.InheritEnvironment(nil)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	process, err := owner.Start(ctx, testprocess.Spec{
		Identity: launch.Identity,
		Command: testprocess.Command{
			Executable: launch.Executable, Arguments: fixture.arguments(),
			WorkingDirectory: launch.WorkingDirectory, Environment: environment,
		},
		Deadline: processDeadline, TerminationGrace: terminationGrace,
	})
	if err != nil {
		return nil, fmt.Errorf("start native relay: %w", err)
	}
	host := &Host{process: process}
	ready, err := awaitReadiness(ctx, process)
	if err == nil {
		err = fixture.acceptReadiness(ready)
	}
	if err != nil {
		// Startup cancellation cannot revoke the independent cleanup lease.
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), terminationGrace)
		defer cancelCleanup()
		return nil, errors.Join(fmt.Errorf("native relay readiness: %w; %s", err, host.Diagnostic()), host.Close(cleanup))
	}
	host.Ready = ready
	return host, nil
}

func (fixture *Fixture) arguments() []string {
	arguments := []string{
		"--listen", fixture.listenAddress, "--database", fixture.database,
		// Default STUN ports would couple otherwise isolated parallel scenarios.
		"--stun-udp", loopbackAddress, "--stun-admin", loopbackAddress,
	}
	if fixture.publicEndpoint != "" {
		arguments = append(arguments, "--public-endpoint", fixture.publicEndpoint)
	}
	return arguments
}

func awaitReadiness(ctx context.Context, process *testprocess.Process) (Readiness, error) {
	match, err := process.WaitForOutput(ctx, testprocess.Stdout, readinessLine)
	if err != nil {
		return Readiness{}, err
	}
	return parseReadiness(match[1])
}

func (fixture *Fixture) acceptReadiness(ready Readiness) error {
	if fixture.publicEndpoint == "" {
		// Direct restarts retain the socket as well as the signed identity.
		// A proxy retains only its public identity and may use a fresh backend.
		fixture.listenAddress = ready.Address
		fixture.publicEndpoint = ready.Endpoint
	} else if ready.Endpoint != fixture.publicEndpoint {
		return fmt.Errorf("native relay endpoint changed: got %q, want %q", ready.Endpoint, fixture.publicEndpoint)
	}
	return nil
}

func (host *Host) Stdout() testprocess.OutputSnapshot { return host.process.Stdout() }
func (host *Host) Stderr() testprocess.OutputSnapshot { return host.process.Stderr() }
func (host *Host) Diagnostic() string                 { return host.process.Diagnostic() }

func (host *Host) Close(ctx context.Context) error {
	result, err := host.process.Stop(ctx)
	if err := testprocess.RequireClean(result, err); err != nil {
		return fmt.Errorf("retire native relay process tree: %w; %s", err, host.Diagnostic())
	}
	return nil
}
