//go:build windows

package processowner_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testprocess"
)

func TestWindowsSpawnFailureDrainsMaximumRequestInput(t *testing.T) {
	trace, identity := startProcessOwnerScenario(t, "integration/processowner/windows-spawn-input")
	repositoryRoot := testRepositoryRoot(t)
	owner := integrationOwner(t, repositoryRoot)
	environment, err := testprocess.InheritEnvironment(nil)
	if err != nil {
		t.Fatal(err)
	}
	process, err := owner.Start(context.Background(), testprocess.Spec{
		Identity: identity,
		Command: testprocess.Command{
			Executable:       filepath.Join(t.TempDir(), "missing-target.exe"),
			Arguments:        []string{},
			WorkingDirectory: repositoryRoot,
			Environment:      environment,
			Stdin:            bytes.Repeat([]byte{0x6b}, protocol.MaximumStdinBytes),
		},
		Deadline:         10 * time.Second,
		TerminationGrace: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	trace.RequireCleanup(t, "spawn-failure owner", func(cleanupContext context.Context) error {
		_, cleanupErr := process.StopAndRequireTreeEmpty(cleanupContext)
		return cleanupErr
	})
	waitContext, cancelWait := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelWait()
	settlement, waitErr := process.Wait(waitContext)
	if waitErr != nil {
		t.Fatalf("wait for spawn failure: %v", waitErr)
	}
	if err := testprocess.RequireTreeEmpty(settlement); err != nil {
		t.Fatal(err)
	}
	if settlement.Target.Outcome != protocol.TargetSpawnFailed ||
		settlement.Input.Outcome != protocol.InputNotStarted ||
		settlement.TerminationReason != protocol.TerminationInitializationFailed {
		t.Fatalf("spawn-failure settlement = %#v", settlement)
	}
	finishProcessOwnerScenario(t, trace)
}

func TestWindowsBoundedOutputCapturePreservesNaturalSettlement(t *testing.T) {
	trace, identity := startProcessOwnerScenario(t, "integration/processowner/windows-output-capture")
	repositoryRoot := testRepositoryRoot(t)
	owner := integrationOwner(t, repositoryRoot)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environment, err := ownedFixtureEnvironment(identity, "verbose-output")
	if err != nil {
		t.Fatal(err)
	}
	process, err := owner.Start(context.Background(), testprocess.Spec{
		Identity: identity,
		Command: testprocess.Command{
			Executable:       executable,
			Arguments:        []string{"-test.run=^TestOwnedProcessFixture$", "-test.count=1"},
			WorkingDirectory: repositoryRoot,
			Environment:      environment,
		},
		Deadline:         20 * time.Second,
		TerminationGrace: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	trace.RequireCleanup(t, "output-capture owner", func(cleanupContext context.Context) error {
		cleanupSettlement, cleanupErr := process.Stop(cleanupContext)
		if cleanupErr != nil && !errors.Is(cleanupErr, testprocess.ErrOutputCaptureLimit) {
			return cleanupErr
		}
		return testprocess.RequireTreeEmpty(cleanupSettlement)
	})
	waitContext, cancelWait := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelWait()
	settlement, waitErr := process.Wait(waitContext)
	if !errors.Is(waitErr, testprocess.ErrOutputCaptureLimit) {
		t.Fatalf("output capture error = %v", waitErr)
	}
	if err := testprocess.RequireTreeEmpty(settlement); err != nil {
		t.Fatal(err)
	}
	if settlement.TerminationReason != protocol.TerminationNatural ||
		settlement.Target.Outcome != protocol.TargetExited || settlement.Target.ExitCode == nil ||
		*settlement.Target.ExitCode != 0 || settlement.Input.Outcome != protocol.InputNotRequested {
		t.Fatalf("natural settlement after output capture limit = %#v", settlement)
	}
	stdout := process.Stdout()
	if !stdout.Truncated || len(stdout.Bytes) != testprocess.MaximumCapturedOutputBytes {
		t.Fatalf("bounded stdout snapshot = bytes=%d truncated=%t", len(stdout.Bytes), stdout.Truncated)
	}
	again, againErr := process.Wait(waitContext)
	if !errors.Is(againErr, testprocess.ErrOutputCaptureLimit) || again.Identity != identity || againErr.Error() != waitErr.Error() {
		t.Fatalf("repeated Wait changed settlement/error: %#v, %v", again, againErr)
	}
	finishProcessOwnerScenario(t, trace)
}
