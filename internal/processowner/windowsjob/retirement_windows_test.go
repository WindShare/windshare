//go:build windows

package windowsjob

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRetirementUsesKernelCompletionAtBudgetBoundary(t *testing.T) {
	for _, mode := range []string{"natural", "forced"} {
		t.Run(mode, func(t *testing.T) {
			targetMode := "natural"
			wantExitCode := int64(0)
			if mode == "forced" {
				targetMode = "deadline"
				wantExitCode = int64(forcedTerminationCode)
			}
			job, root := startRetirementTarget(t, targetMode)
			if mode == "forced" {
				// Containment, rather than application signal handling, owns
				// forced termination and the resulting exit code.
				if err := windows.TerminateJobObject(job, forcedTerminationCode); err != nil {
					t.Fatal(err)
				}
			}
			observed := waitRetirementTarget(t, job, root)
			if observed.exitCode != wantExitCode {
				t.Fatalf("root exit code = %d, want %d", observed.exitCode, wantExitCode)
			}

			// The host may resume the owner after the cleanup budget expires.
			// Already observable completion must not require more timer ticks.
			if mode == "natural" {
				if err := retireJob(job, 0); err != nil {
					t.Fatal(err)
				}
			} else {
				result, err := retireForcedJob(job, root, 0)
				if err != nil || result.err != nil || result.exitCode != wantExitCode {
					t.Fatalf("forced retirement: result=%+v cleanup=%v", result, err)
				}
			}
		})
	}
}

func TestForcedRetirementDoesNotReportALiveRootAsSettled(t *testing.T) {
	job, root := startRetirementTarget(t, "deadline")
	result, err := retireForcedJob(job, root, 0)
	if err == nil || result.err == nil || result.exitCode != -1 ||
		!strings.Contains(err.Error(), "root_settled=false") {
		t.Fatalf("live root retirement: result=%+v cleanup=%v", result, err)
	}
}

func TestForcedRetirementRequiresBothKernelFactsAtBudgetBoundary(t *testing.T) {
	for _, test := range []struct {
		name          string
		rootSettlesAt int
		active        uint32
		wantRootCalls int
		wantCleanup   bool
		wantRootError bool
	}{
		{name: "empty job before root signals", wantRootCalls: 2, wantCleanup: true, wantRootError: true},
		{name: "root signals after empty accounting", rootSettlesAt: 2, wantRootCalls: 2},
		{name: "settled root and empty job", rootSettlesAt: 1, wantRootCalls: 1},
		{name: "settled root with remaining descendants", rootSettlesAt: 1, active: 1, wantRootCalls: 1, wantCleanup: true},
		{name: "live root and descendants", active: 1, wantRootCalls: 1, wantCleanup: true, wantRootError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootCalls, activeCalls := 0, 0
			result, cleanupErr := awaitForcedRetirement(
				0,
				func() (rootResult, bool) {
					rootCalls++
					settled := test.rootSettlesAt > 0 && rootCalls >= test.rootSettlesAt
					return rootResult{exitCode: int64(forcedTerminationCode)}, settled
				},
				func() (uint32, error) {
					activeCalls++
					return test.active, nil
				},
			)
			if (cleanupErr != nil) != test.wantCleanup || (result.err != nil) != test.wantRootError {
				t.Fatalf("retirement: result=%+v cleanup=%v", result, cleanupErr)
			}
			wantExitCode := int64(forcedTerminationCode)
			if test.wantRootError {
				wantExitCode = -1
			}
			if result.exitCode != wantExitCode || rootCalls != test.wantRootCalls || activeCalls != 1 {
				t.Fatalf("retirement: result=%+v root_calls=%d active_calls=%d", result, rootCalls, activeCalls)
			}
		})
	}
}

func TestForcedRetirementPreservesObservationErrors(t *testing.T) {
	rootErr := errors.New("root observation failed")
	accountingErr := errors.New("accounting observation failed")
	for _, settled := range []bool{false, true} {
		result, cleanupErr := awaitForcedRetirement(
			0,
			func() (rootResult, bool) { return rootResult{exitCode: -1, err: rootErr}, settled },
			func() (uint32, error) { return 0, accountingErr },
		)
		if !errors.Is(cleanupErr, accountingErr) || result.err == nil || result.exitCode != -1 {
			t.Fatalf("retirement: settled=%t result=%+v cleanup=%v", settled, result, cleanupErr)
		}
		if settled && !errors.Is(result.err, rootErr) {
			t.Fatalf("root observation error was lost: %v", result.err)
		}
	}
}

func startRetirementTarget(t *testing.T, mode string) (windows.Handle, rootProcess) {
	t.Helper()
	job, err := createJob()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := windows.CloseHandle(job); err != nil {
			t.Error(err)
		}
	})
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeTestFiles(reader, writer); err != nil {
			t.Error(err)
		}
	})
	config := windowsSupervisorConfig(t, mode, time.Second, time.Second)
	root, err := startTarget(config, job, writer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer func() {
			if err := windows.CloseHandle(root.handle); err != nil {
				t.Error(err)
			}
		}()
		if err := windows.TerminateJobObject(job, forcedTerminationCode); err != nil {
			t.Error(err)
		}
		waitRetirementTarget(t, job, root)
	})
	return job, root
}

func waitRetirementTarget(t *testing.T, job windows.Handle, root rootProcess) rootResult {
	t.Helper()
	deadline := time.Now().Add(windowsSupervisorStartupTimeout)
	observed, settled := waitRootFor(root, windowsSupervisorStartupTimeout)
	if !settled || observed.err != nil {
		t.Fatalf("root did not settle: result=%+v settled=%t", observed, settled)
	}
	// Job termination is asynchronous for each member. Root exit alone does
	// not establish the completed-job prerequisite of the zero-budget test.
	for {
		active, err := activeProcessCount(job)
		if err != nil {
			t.Fatal(err)
		}
		if active == 0 {
			return observed
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("target Job Object did not empty: active_processes=%d", active)
		}
		time.Sleep(min(jobPollInterval, remaining))
	}
}
