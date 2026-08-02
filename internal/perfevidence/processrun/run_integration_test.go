package processrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

const (
	processrunTargetModeEnvironment = "WINDSHARE_PROCESSRUN_TEST_TARGET_MODE"
	processrunMarkerEnvironment     = "WINDSHARE_PROCESSRUN_TEST_MARKER"
	processrunReadyEnvironment      = "WINDSHARE_PROCESSRUN_TEST_READY"
	processrunTargetDelay           = 700 * time.Millisecond
	processrunTestLease             = 10 * time.Second
)

type processrunOutcome struct {
	result Result
	err    error
}

func TestMain(suite *testing.M) {
	if handled, code := MaybeRunHelper(os.Args[1:], os.Stdin); handled {
		os.Exit(code)
	}
	if mode := os.Getenv(processrunTargetModeEnvironment); mode != "" {
		os.Exit(runProcessrunTarget(mode))
	}
	os.Exit(suite.Run())
}

func TestRunnerUsesContainedOwnerForNaturalAndNonzeroExit(t *testing.T) {
	t.Run("natural", func(t *testing.T) {
		spec := processrunTestSpec(t, "success", nil)
		result, err := (Runner{}).Run(context.Background(), spec)
		if err != nil {
			t.Fatalf(
				"natural failure: error=%v reason=%s target=%s owner=%#v cleanup=%#v stderr=%q",
				err,
				result.Settlement.TerminationReason,
				result.Settlement.Target.Outcome,
				result.Settlement.OwnerFailure,
				result.Settlement.Cleanup,
				result.Stderr,
			)
		}
		if string(result.Stdout) != "owned-stdout\n" || string(result.Stderr) != "owned-stderr\n" ||
			result.ProcessID < 1 || result.ExitCode != 0 ||
			result.Settlement.TreeState != protocol.TreeProvenEmpty ||
			result.Settlement.Cleanup.Outcome != protocol.CleanupCompleted {
			t.Fatalf("natural result = %#v", result)
		}
	})

	t.Run("nonzero after settlement", func(t *testing.T) {
		spec := processrunTestSpec(t, "nonzero", nil)
		result, err := (Runner{}).Run(context.Background(), spec)
		var exitErr *ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != 23 || result.ExitCode != 23 ||
			result.Settlement.TreeState != protocol.TreeProvenEmpty ||
			result.Settlement.Cleanup.Outcome != protocol.CleanupCompleted {
			t.Fatalf("nonzero result = %#v, error %v", result, err)
		}
	})
}

func TestRunnerRejectsStartAuthorityBeforeTargetExecution(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	cause := errors.New("live executable identity changed")
	spec := processrunTestSpec(t, "write-marker", map[string]string{
		processrunMarkerEnvironment: marker,
	})
	spec.AuthorizeStart = func(protocol.StartEvidence) error { return cause }
	result, err := (Runner{}).Run(context.Background(), spec)
	if !errors.Is(err, cause) {
		t.Fatalf("authority rejection error = %v, want %v", err, cause)
	}
	if result.Settlement.TerminationReason != protocol.TerminationStartRejected ||
		result.Settlement.Target.Outcome != protocol.TargetNotStarted ||
		result.Settlement.OwnerFailure != nil ||
		result.Settlement.TreeState != protocol.TreeProvenEmpty {
		t.Fatalf("authority rejection settlement = %#v", result.Settlement)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected target executed: %v", statErr)
	}
}

func TestRunnerBoundsAggregateOutputAndStillSettlesTree(t *testing.T) {
	const captureLimit = 1024
	spec := processrunTestSpec(t, "flood", nil)
	result, err := (Runner{MaximumOutput: captureLimit}).Run(context.Background(), spec)
	if !errors.Is(err, ErrOutputCaptureLimit) {
		t.Fatalf("output limit error = %v", err)
	}
	if captured := len(result.Stdout) + len(result.Stderr); captured != captureLimit {
		t.Fatalf("aggregate captured bytes = %d, want %d", captured, captureLimit)
	}
	if result.Settlement.TreeState != protocol.TreeProvenEmpty ||
		result.Settlement.Cleanup.Outcome != protocol.CleanupCompleted {
		t.Fatalf("overflow cleanup = %#v", result.Settlement)
	}
}

func TestRunnerWaitsForNaturalDescendantCompletion(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-finished")
	spec := processrunTestSpec(t, "spawn-natural-descendant", map[string]string{
		processrunMarkerEnvironment: marker,
	})
	started := time.Now()
	result, err := (Runner{}).Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < processrunTargetDelay/2 {
		t.Fatalf("owner returned before descendant completion: %v", elapsed)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("descendant completion marker: %v", err)
	}
	if result.Settlement.TreeState != protocol.TreeProvenEmpty {
		t.Fatalf("descendant settlement = %#v", result.Settlement)
	}
}

func TestRunnerCancellationSettlesWholeDescendantTree(t *testing.T) {
	directory := t.TempDir()
	ready := filepath.Join(directory, "root-ready")
	survivor := filepath.Join(directory, "descendant-survived")
	spec := processrunTestSpec(t, "spawn-cancellable-tree", map[string]string{
		processrunMarkerEnvironment: survivor,
		processrunReadyEnvironment:  ready,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan processrunOutcome, 1)
	go func() {
		result, err := (Runner{}).Run(ctx, spec)
		done <- processrunOutcome{result: result, err: err}
	}()
	waitForProcessrunPath(t, ready, done)
	cancel()

	var completed processrunOutcome
	select {
	case completed = <-done:
	case <-time.After(processrunTestLease):
		t.Fatal("cancelled process tree did not settle")
	}
	if !errors.Is(completed.err, context.Canceled) {
		t.Fatalf("cancellation error = %v", completed.err)
	}
	if completed.result.Settlement.TreeState != protocol.TreeProvenEmpty ||
		completed.result.Settlement.Cleanup.Outcome != protocol.CleanupCompleted {
		t.Fatalf("cancelled tree settlement = %#v", completed.result.Settlement)
	}
	time.Sleep(processrunTargetDelay + 100*time.Millisecond)
	if _, err := os.Stat(survivor); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descendant survived settled cancellation: %v", err)
	}
}

func TestRunnerTreatsInjectedCleanupFailureAsHard(t *testing.T) {
	cause := errors.New("external cleanup proof rejected")
	spec := processrunTestSpec(t, "success", nil)
	result, err := (Runner{ValidateCleanup: func(protocol.Settlement, protocol.Request) error {
		return cause
	}}).Run(context.Background(), spec)
	if !errors.Is(err, cause) || result.Settlement.TreeState != protocol.TreeProvenEmpty {
		t.Fatalf("cleanup validator result = %#v, error %v", result, err)
	}
}

func TestRunnerRequiresAuthorizerBeforeLaunch(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	spec := processrunTestSpec(t, "write-marker", map[string]string{
		processrunMarkerEnvironment: marker,
	})
	spec.AuthorizeStart = nil
	if _, err := (Runner{}).Run(context.Background(), spec); err == nil {
		t.Fatal("nil start authorizer was accepted")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target launched without an authorizer: %v", err)
	}
}

func TestBoundedDiagnosticPreservesUTF8AtBoundary(t *testing.T) {
	message := strings.Repeat("a", protocol.MaximumDiagnosticBytes-1) + "🙂\x00"
	bounded := boundedDiagnostic(errors.New(message))
	if len(bounded) > protocol.MaximumDiagnosticBytes || !utf8.ValidString(bounded) || strings.ContainsRune(bounded, 0) {
		t.Fatalf("bounded diagnostic = %q (%d bytes)", bounded, len(bounded))
	}
}

func processrunTestSpec(t *testing.T, mode string, values map[string]string) Spec {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	overrides := []string{processrunTargetModeEnvironment + "=" + mode}
	for name, value := range values {
		overrides = append(overrides, name+"="+value)
	}
	environment, err := CanonicalEnvironment(os.Environ(), overrides)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return Spec{
		Identity:         identity,
		Executable:       filepath.Clean(executable),
		Arguments:        []string{},
		WorkingDirectory: filepath.Dir(executable),
		Environment:      environment,
		Deadline:         5 * time.Second,
		TerminationGrace: 2 * time.Second,
		AuthorizeStart:   func(protocol.StartEvidence) error { return nil },
	}
}

func runProcessrunTarget(mode string) int {
	switch mode {
	case "success":
		_, _ = fmt.Fprintln(os.Stdout, "owned-stdout")
		_, _ = fmt.Fprintln(os.Stderr, "owned-stderr")
		return 0
	case "nonzero":
		return 23
	case "write-marker":
		if err := os.WriteFile(os.Getenv(processrunMarkerEnvironment), []byte("executed\n"), 0o600); err != nil {
			return 91
		}
		return 0
	case "flood":
		payload := bytes.Repeat([]byte("x"), 4<<10)
		_, _ = os.Stdout.Write(payload)
		_, _ = os.Stderr.Write(payload)
		return 0
	case "spawn-natural-descendant":
		if err := startProcessrunDescendant("delayed-marker", os.Getenv(processrunMarkerEnvironment)); err != nil {
			return 92
		}
		return 0
	case "spawn-cancellable-tree":
		if err := startProcessrunDescendant("delayed-marker", os.Getenv(processrunMarkerEnvironment)); err != nil {
			return 93
		}
		if err := os.WriteFile(os.Getenv(processrunReadyEnvironment), []byte("ready\n"), 0o600); err != nil {
			return 94
		}
		for {
			time.Sleep(time.Hour)
		}
	case "delayed-marker":
		time.Sleep(processrunTargetDelay)
		if err := os.WriteFile(os.Getenv(processrunMarkerEnvironment), []byte("survived\n"), 0o600); err != nil {
			return 95
		}
		return 0
	default:
		return 96
	}
}

func startProcessrunDescendant(mode, marker string) error {
	command := exec.Command(os.Args[0])
	command.Env = replaceProcessrunTestEnvironment(os.Environ(), map[string]string{
		processrunTargetModeEnvironment: mode,
		processrunMarkerEnvironment:     marker,
	})
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func replaceProcessrunTestEnvironment(environment []string, replacements map[string]string) []string {
	result := make([]string, 0, len(environment)+len(replacements))
	for _, assignment := range environment {
		name, _, found := strings.Cut(assignment, "=")
		if !found {
			continue
		}
		replaced := false
		for candidate := range replacements {
			if strings.EqualFold(name, candidate) {
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, assignment)
		}
	}
	for name, value := range replacements {
		result = append(result, name+"="+value)
	}
	return result
}

func waitForProcessrunPath(t *testing.T, path string, done <-chan processrunOutcome) {
	t.Helper()
	deadline := time.Now().Add(processrunTestLease)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case completed := <-done:
			t.Fatalf("owned target exited before readiness: result=%#v error=%v", completed.result, completed.err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("owned target did not publish readiness at %s", path)
}
