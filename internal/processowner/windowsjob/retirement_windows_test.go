//go:build windows

package windowsjob

import (
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
