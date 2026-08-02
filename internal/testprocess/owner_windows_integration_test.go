//go:build windows

package testprocess

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/processowner/windowsjob"
)

const windowsOwnerTargetModeEnvironment = "WINDSHARE_TESTPROCESS_WINDOWS_TARGET_MODE"

func TestMain(suite *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "supervise" || os.Args[1] == "launcher") {
		if err := windowsjob.Run(os.Args[1:], os.Stdin); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if mode := os.Getenv(windowsOwnerTargetModeEnvironment); mode != "" {
		os.Exit(runWindowsOwnerTarget(mode))
	}
	os.Exit(suite.Run())
}

func TestWindowsOwnerRunsRequestBoundLifecycles(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(executable)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Errorf("close owner: %v", err)
		}
	})

	t.Run("natural exact input and streams", func(t *testing.T) {
		identity := ownerprotocol.Identity{RunID: "windows-owner", OperationID: "natural", Scenario: "real-lifecycle"}
		input := bytes.Repeat([]byte("exact-input"), 32<<10)
		process := startWindowsOwnerFixture(t, owner, executable, identity, "echo", input, 10*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		settlement, waitErr := process.Wait(ctx)
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		if err := RequireTreeEmpty(settlement); err != nil {
			t.Fatal(err)
		}
		stdout, stderr := process.Stdout(), process.Stderr()
		if !bytes.Equal(stdout.Bytes, input) || stderr.String() != "fixture-stderr" ||
			settlement.Input.Outcome != ownerprotocol.InputDelivered {
			t.Fatalf("natural lifecycle: stdout=%d stderr=%q settlement=%#v", len(stdout.Bytes), stderr.String(), settlement)
		}
		if _, eventErr := process.Events().Next(ctx); !errors.Is(eventErr, io.EOF) {
			t.Fatalf("empty event stream = %v", eventErr)
		}
	})

	t.Run("authenticated stop", func(t *testing.T) {
		identity := ownerprotocol.Identity{RunID: "windows-owner", OperationID: "stop", Scenario: "real-lifecycle"}
		process := startWindowsOwnerFixture(t, owner, executable, identity, "block", nil, 20*time.Second)
		readyDeadline := time.Now().Add(5 * time.Second)
		for !bytes.Contains(process.Stdout().Bytes, []byte("ready\n")) {
			if time.Now().After(readyDeadline) {
				t.Fatalf("blocking fixture did not become ready; stdout=%q stderr=%q", process.Stdout().String(), process.Stderr().String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		settlement, stopErr := process.Stop(ctx)
		if stopErr != nil {
			t.Fatal(stopErr)
		}
		if err := RequireTreeEmpty(settlement); err != nil {
			t.Fatal(err)
		}
		if settlement.TerminationReason != ownerprotocol.TerminationStop {
			t.Fatalf("stop termination reason = %q", settlement.TerminationReason)
		}
	})

	t.Run("spawn failure drains maximum input", func(t *testing.T) {
		identity := ownerprotocol.Identity{RunID: "windows-owner", OperationID: "spawn-failure", Scenario: "real-lifecycle"}
		environment, environmentErr := InheritEnvironment(nil)
		if environmentErr != nil {
			t.Fatal(environmentErr)
		}
		process, startErr := owner.Start(context.Background(), Spec{
			Identity: identity,
			Command: Command{
				Executable: filepath.Join(t.TempDir(), "missing.exe"), Arguments: []string{},
				WorkingDirectory: t.TempDir(), Environment: environment,
				Stdin: bytes.Repeat([]byte{0x43}, ownerprotocol.MaximumStdinBytes),
			},
			Deadline: 10 * time.Second, TerminationGrace: 2 * time.Second,
		})
		if startErr != nil {
			t.Fatal(startErr)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		settlement, waitErr := process.Wait(ctx)
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		if err := RequireTreeEmpty(settlement); err != nil {
			t.Fatal(err)
		}
		if settlement.Target.Outcome != ownerprotocol.TargetSpawnFailed || settlement.Input.Outcome != ownerprotocol.InputNotStarted {
			t.Fatalf("spawn-failure settlement = %#v", settlement)
		}
	})
}

func startWindowsOwnerFixture(
	t *testing.T,
	owner *Owner,
	executable string,
	identity ownerprotocol.Identity,
	mode string,
	stdin []byte,
	deadline time.Duration,
) *Process {
	t.Helper()
	environment, err := InheritEnvironment(map[string]string{windowsOwnerTargetModeEnvironment: mode})
	if err != nil {
		t.Fatal(err)
	}
	process, err := owner.Start(context.Background(), Spec{
		Identity: identity,
		Command: Command{
			Executable: executable, Arguments: []string{"-test.run=^$"}, WorkingDirectory: t.TempDir(),
			Environment: environment, Stdin: stdin,
		},
		Deadline: deadline, TerminationGrace: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return process
}

func runWindowsOwnerTarget(mode string) int {
	switch mode {
	case "echo":
		input, err := io.ReadAll(io.LimitReader(os.Stdin, ownerprotocol.MaximumStdinBytes+1))
		if err != nil || len(input) > ownerprotocol.MaximumStdinBytes {
			return 90
		}
		if _, err := os.Stdout.Write(input); err != nil {
			return 91
		}
		if _, err := io.WriteString(os.Stderr, "fixture-stderr"); err != nil {
			return 92
		}
		return 0
	case "block":
		_, _ = io.WriteString(os.Stdout, "ready\n")
		time.Sleep(time.Minute)
		return 0
	default:
		return 93
	}
}
