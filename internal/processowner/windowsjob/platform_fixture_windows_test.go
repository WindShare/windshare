//go:build windows

package windowsjob

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	testHelperEnvironment        = "WINDSHARE_WINDOWSJOB_TEST_HELPER"
	testTargetEnvironment        = "WINDSHARE_WINDOWSJOB_TEST_TARGET"
	testMarkerEnvironment        = "WINDSHARE_WINDOWSJOB_TEST_MARKER"
	testReadyEnvironment         = "WINDSHARE_WINDOWSJOB_TEST_READY"
	testCWDEnvironment           = "WINDSHARE_WINDOWSJOB_TEST_CWD"
	rootNaturalExitCode          = 37
	rootBeforeDescendantExitCode = 7
	launcherReleaseRootExitCode  = 46
)

const (
	testMarkerPollInterval         = 20 * time.Millisecond
	testMarkerWaitLimit            = 10 * time.Second
	rootReadyDelay                 = 300 * time.Millisecond
	launcherReleaseRootDelay       = 750 * time.Millisecond
	naturalDescendantDelay         = 250 * time.Millisecond
	deadlineTestMS           int64 = 3_000
	preReleaseDeadlineMS     int64 = 50
	preReleaseDecisionDelay        = 200 * time.Millisecond
	deadlineDescendantDelay        = 5 * time.Second
	postDeadlineObservation        = 3 * time.Second
	breakawayWriterDelay           = 1 * time.Second
	postBreakawayObservation       = 1_500 * time.Millisecond
	crashDescendantDelay           = 2 * time.Second
	postCrashObservation           = 3 * time.Second
)

func TestMain(testMain *testing.M) {
	if target := os.Getenv(testTargetEnvironment); target != "" {
		os.Exit(runWindowsTargetFixture(target))
	}
	if os.Getenv(testHelperEnvironment) == "1" {
		if err := runCommand(os.Args[1:], os.Stdin); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(testMain.Run())
}

func runWindowsTargetFixture(mode string) int {
	switch mode {
	case "echo":
		cwd, err := os.Getwd()
		if err != nil || filepath.Clean(cwd) != filepath.Clean(os.Getenv(testCWDEnvironment)) {
			return 91
		}
		_, _ = os.Stdout.Write([]byte("stdout\x00fake-status-frame\n" + strings.Join(os.Args[1:], "\x1f")))
		_, _ = os.Stderr.Write([]byte("stderr<>&\u2028"))
		return rootNaturalExitCode
	case "stdin":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 103
		}
		defer func() {
			for index := range input {
				input[index] = 0
			}
		}()
		if _, err := os.Stdout.Write(input); err != nil {
			return 104
		}
		return rootNaturalExitCode
	case "stdin-silent":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 105
		}
		for index := range input {
			input[index] = 0
		}
		return rootNaturalExitCode
	case "exit-259":
		windows.ExitProcess(259)
		return 259
	case "launcher-release-root":
		time.Sleep(launcherReleaseRootDelay)
		return launcherReleaseRootExitCode
	case "unexpected-release":
		if err := os.WriteFile(os.Getenv(testMarkerEnvironment), []byte("released"), 0o600); err != nil {
			return 106
		}
		return 0
	case "natural-descendant":
		if err := startLateWriter(naturalDescendantDelay); err != nil {
			return 92
		}
		return rootBeforeDescendantExitCode
	case "deadline-descendant":
		if err := startLateWriter(deadlineDescendantDelay); err != nil {
			return 93
		}
		if err := os.WriteFile(os.Getenv(testReadyEnvironment), []byte("descendant-started"), 0o600); err != nil {
			return 99
		}
		return rootBeforeDescendantExitCode
	case "crash-tree":
		if err := startLateWriter(crashDescendantDelay); err != nil {
			return 101
		}
		if err := os.WriteFile(os.Getenv(testReadyEnvironment), []byte("tree-ready"), 0o600); err != nil {
			return 102
		}
		time.Sleep(10 * time.Second)
		return 0
	case "late-writer":
		delay, err := time.ParseDuration(os.Getenv("WINDSHARE_WINDOWSJOB_TEST_DELAY"))
		if err != nil {
			return 94
		}
		time.Sleep(delay)
		if err := os.WriteFile(os.Getenv(testMarkerEnvironment), []byte(os.Getenv("WINDSHARE_WINDOWSJOB_TEST_MARKER_VALUE")), 0o600); err != nil {
			return 95
		}
		return 0
	case "breakaway-attempt":
		if err := startBreakawayWriter(); err == nil {
			return 96
		}
		if err := os.WriteFile(os.Getenv(testMarkerEnvironment), []byte("breakaway-blocked"), 0o600); err != nil {
			return 97
		}
		return 0
	case "long-root":
		time.Sleep(rootReadyDelay)
		if err := os.WriteFile(os.Getenv(testMarkerEnvironment), []byte("root-ready"), 0o600); err != nil {
			return 100
		}
		time.Sleep(10 * time.Second)
		return 0
	default:
		return 98
	}
}

func startLateWriter(delay time.Duration) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable)
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		testTargetEnvironment:                    "late-writer",
		"WINDSHARE_WINDOWSJOB_TEST_DELAY":        delay.String(),
		"WINDSHARE_WINDOWSJOB_TEST_MARKER_VALUE": "natural-descendant",
	})
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func startBreakawayWriter() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable)
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		testTargetEnvironment:                    "late-writer",
		"WINDSHARE_WINDOWSJOB_TEST_DELAY":        breakawayWriterDelay.String(),
		"WINDSHARE_WINDOWSJOB_TEST_MARKER_VALUE": "escaped",
	})
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB,
	}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func replaceEnvironment(environment []string, replacements map[string]string) []string {
	result := make([]string, 0, len(environment)+len(replacements))
	for _, assignment := range environment {
		separator := strings.IndexByte(assignment, '=')
		if separator < 1 {
			continue
		}
		name := assignment[:separator]
		replaced := false
		for replacement := range replacements {
			if strings.EqualFold(name, replacement) {
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
