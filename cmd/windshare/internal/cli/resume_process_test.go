package cli

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const resumeProcessHelperEnvironment = "WINDSHARE_RESUME_PROCESS_HELPER"

func TestResumeHelpProductionProcessExposesOnlyCapabilityCommands(t *testing.T) {
	if os.Getenv(resumeProcessHelperEnvironment) == "1" {
		os.Args = []string{"windshare", "resume", "help"}
		os.Exit(Main())
	}

	command := exec.Command(os.Args[0], "-test.run=^TestResumeHelpProductionProcessExposesOnlyCapabilityCommands$")
	command.Env = append(
		os.Environ(),
		resumeProcessHelperEnvironment+"=1",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("run resume help process: %v stderr=%q", err, stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "windshare resume list") ||
		!strings.Contains(stderr.String(), "windshare resume discard") ||
		!strings.Contains(stderr.String(), "operation-needs-attention") ||
		!strings.Contains(stderr.String(), "item-blocked") ||
		strings.Contains(stderr.String(), "resume cleanup") ||
		strings.Contains(stderr.String(), "legacy") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
