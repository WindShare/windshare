//go:build linux || windows

package testprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testtrace"
)

const (
	ownedProbeEnvironment         = "WINDSHARE_TESTPROCESS_PROBE"
	ownedProbeComponent           = testrun.Component("process_probe")
	ownedProbeReady               = testrun.Milestone("probe_ready")
	ownedProbeStopped             = testrun.Milestone("probe_stopped")
	ownedProbeTerminalOutputBytes = 256 << 10
	ownedProbeStdoutTerminal      = "\nprobe stdout terminal\n"
	ownedProbeStderrTerminal      = "\nprobe stderr terminal\n"
)

func TestOwnerSupervisesLifecycleAndDescendants(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	buildContext, cancelBuild := context.WithTimeout(t.Context(), time.Minute)
	defer cancelBuild()
	owner, err := BuildOwner(buildContext, repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	})

	t.Run("natural exit captures output and event", func(t *testing.T) {
		process := startOwnedProbe(t, owner, "exit", 10*time.Second, time.Second)
		readyContext, cancelReady := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancelReady()
		match, err := process.WaitForOutput(readyContext, Stdout, regexp.MustCompile(`probe ready`))
		if err != nil || len(match) == 0 {
			t.Fatalf("stdout readiness = %v, %v", match, err)
		}
		event, err := process.Events().Next(readyContext)
		if err != nil || event.Milestone != string(ownedProbeReady) {
			t.Fatalf("ready event = %+v, %v", event, err)
		}
		result, err := process.Wait(readyContext)
		if err := RequireSuccess(result, err); err != nil {
			t.Fatal(err)
		}
		if result.Reason != processowner.ReasonNatural ||
			!strings.Contains(process.Stdout().String(), "probe ready") ||
			!strings.Contains(process.Stderr().String(), "probe diagnostic") {
			t.Fatalf("result=%+v stdout=%q stderr=%q", result, process.Stdout().String(), process.Stderr().String())
		}
		if diagnostic := process.Diagnostic(); !strings.Contains(diagnostic, "operation-exit") {
			t.Fatalf("diagnostic = %q", diagnostic)
		}
		if _, err := process.Events().Next(readyContext); !errors.Is(err, io.EOF) {
			t.Fatalf("event stream end = %v", err)
		}
	})

	t.Run("terminal output drains before lifecycle completion", func(t *testing.T) {
		process := startOwnedProbe(t, owner, "terminal-output", 10*time.Second, time.Second)
		waitContext, cancelWait := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancelWait()
		result, err := process.Wait(waitContext)
		if err := RequireSuccess(result, err); err != nil {
			t.Fatal(err)
		}
		for _, captured := range []struct {
			stream   OutputStream
			snapshot OutputSnapshot
			payload  []byte
		}{
			{stream: Stdout, snapshot: process.Stdout(), payload: ownedProbeTerminalPayload('o', ownedProbeStdoutTerminal)},
			{stream: Stderr, snapshot: process.Stderr(), payload: ownedProbeTerminalPayload('e', ownedProbeStderrTerminal)},
		} {
			if captured.snapshot.Truncated || !bytes.Contains(captured.snapshot.Bytes, captured.payload) {
				t.Fatalf(
					"%s output length=%d truncated=%v complete_payload=%v",
					captured.stream,
					len(captured.snapshot.Bytes),
					captured.snapshot.Truncated,
					bytes.Contains(captured.snapshot.Bytes, captured.payload),
				)
			}
		}
	})

	t.Run("interrupt reaches target", func(t *testing.T) {
		process := startOwnedProbe(t, owner, "interrupt", 10*time.Second, 2*time.Second)
		waitForOwnedProbe(t, process, `probe waiting`)
		stopContext, cancelStop := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancelStop()
		result, err := process.Interrupt(stopContext)
		if err := RequireSuccess(result, err); err != nil {
			t.Fatal(err)
		}
		if result.Reason != processowner.ReasonInterrupt {
			t.Fatalf("interrupt result = %+v", result)
		}
		event, err := process.Events().Next(stopContext)
		if err != nil || event.Milestone != string(ownedProbeStopped) {
			t.Fatalf("stop event = %+v, %v", event, err)
		}
	})

	t.Run("stop reaches target", func(t *testing.T) {
		process := startOwnedProbe(t, owner, "interrupt", 10*time.Second, 2*time.Second)
		waitForOwnedProbe(t, process, `probe waiting`)
		stopContext, cancelStop := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancelStop()
		result, err := process.Stop(stopContext)
		if err := RequireSuccess(result, err); err != nil {
			t.Fatal(err)
		}
		if result.Reason != processowner.ReasonStop {
			t.Fatalf("stop result = %+v", result)
		}
	})

	t.Run("deadline escalates", func(t *testing.T) {
		process := startOwnedProbe(t, owner, "deadline", 300*time.Millisecond, 100*time.Millisecond)
		waitForOwnedProbe(t, process, `probe ignoring interrupt`)
		waitContext, cancelWait := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancelWait()
		result, err := process.Wait(waitContext)
		if err := RequireClean(result, err); err != nil {
			t.Fatal(err)
		}
		if result.Reason != processowner.ReasonDeadline || result.ExitCode == nil || *result.ExitCode == 0 {
			t.Fatalf("deadline result = %+v", result)
		}
	})

	t.Run("natural root exit retires descendant", func(t *testing.T) {
		process := startOwnedProbe(t, owner, "descendant-root", 10*time.Second, time.Second)
		readyContext, cancelReady := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancelReady()
		match, err := process.WaitForOutput(
			readyContext,
			Stdout,
			regexp.MustCompile(`descendant pid=(\d+)`),
		)
		if err != nil {
			t.Fatal(err)
		}
		processID, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatal(err)
		}
		result, err := process.Wait(readyContext)
		if err := RequireSuccess(result, err); err != nil {
			t.Fatal(err)
		}
		if err := requireProcessGone(processID); err != nil {
			t.Fatal(err)
		}
	})
}

func startOwnedProbe(
	t *testing.T,
	owner *Owner,
	mode string,
	deadline time.Duration,
	grace time.Duration,
) *Process {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := InheritEnvironment(map[string]string{ownedProbeEnvironment: mode})
	if err != nil {
		t.Fatal(err)
	}
	process, err := owner.Start(t.Context(), Spec{
		Identity: testrun.Identity{
			RunID: "process-owner-test", OperationID: "operation-" + strings.ReplaceAll(mode, "-", "_"),
			Scenario: "process-owner/" + mode,
		},
		Command: Command{
			Executable: executable, Arguments: []string{"-test.run=^TestOwnedProcessProbe$"},
			WorkingDirectory: filepath.Dir(executable), Environment: environment,
		},
		Deadline: deadline, TerminationGrace: grace,
	})
	if err != nil {
		t.Fatal(err)
	}
	return process
}

func waitForOwnedProbe(t *testing.T, process *Process, expression string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := process.WaitForOutput(ctx, Stdout, regexp.MustCompile(expression)); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedProcessProbe(t *testing.T) {
	mode := os.Getenv(ownedProbeEnvironment)
	if mode == "" {
		t.Skip("runs only as an owned child process")
	}
	switch mode {
	case "exit":
		fmt.Println("probe ready")
		_, _ = fmt.Fprintln(os.Stderr, "probe diagnostic")
		recordOwnedProbeEvent(t, ownedProbeReady)
	case "terminal-output":
		writeOwnedProbeTerminalOutput(t)
	case "interrupt":
		interrupts := make(chan os.Signal, 1)
		signal.Notify(interrupts, os.Interrupt)
		defer signal.Stop(interrupts)
		fmt.Println("probe waiting")
		<-interrupts
		recordOwnedProbeEvent(t, ownedProbeStopped)
	case "deadline", "descendant":
		signal.Ignore(os.Interrupt)
		if mode == "deadline" {
			fmt.Println("probe ignoring interrupt")
		}
		for {
			time.Sleep(time.Hour)
		}
	case "descendant-root":
		startDescendantProbe(t)
	default:
		t.Fatalf("unknown owned probe mode %q", mode)
	}
}

func writeOwnedProbeTerminalOutput(t *testing.T) {
	t.Helper()
	stdoutPayload := ownedProbeTerminalPayload('o', ownedProbeStdoutTerminal)
	stderrPayload := ownedProbeTerminalPayload('e', ownedProbeStderrTerminal)
	var stdoutErr, stderrErr error
	var writes sync.WaitGroup
	writes.Add(2)
	go func() {
		defer writes.Done()
		stdoutErr = writeOwnedProbePayload(os.Stdout, stdoutPayload)
	}()
	go func() {
		defer writes.Done()
		stderrErr = writeOwnedProbePayload(os.Stderr, stderrPayload)
	}()
	writes.Wait()
	if err := errors.Join(stdoutErr, stderrErr); err != nil {
		t.Fatal(err)
	}
}

func ownedProbeTerminalPayload(fill byte, terminal string) []byte {
	payload := bytes.Repeat([]byte{fill}, ownedProbeTerminalOutputBytes)
	return append(payload, terminal...)
}

func writeOwnedProbePayload(destination io.Writer, payload []byte) error {
	written, err := destination.Write(payload)
	if err == nil && written != len(payload) {
		return io.ErrShortWrite
	}
	return err
}

func recordOwnedProbeEvent(t *testing.T, milestone testrun.Milestone) {
	t.Helper()
	operation, present, err := testrun.OperationFromEnvironment(os.LookupEnv)
	if err != nil || !present {
		t.Fatalf("owned operation = %v, %v", present, err)
	}
	sink, err := testtrace.OpenEventSink(operation.EventIdentity())
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := testrun.NewRecorder(operation, ownedProbeComponent, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Record(milestone, testrun.OutcomeSucceeded, nil); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
}

func startDescendantProbe(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestOwnedProcessProbe$")
	command.Env = replaceProbeMode(os.Environ(), "descendant")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("descendant pid=%d\n", command.Process.Pid)
}

func replaceProbeMode(environment []string, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(name, ownedProbeEnvironment) {
			result = append(result, entry)
		}
	}
	return append(result, ownedProbeEnvironment+"="+value)
}
