//go:build windows

package windowsjob

import (
	"os"
	"os/signal"
	"testing"
)

const (
	windowsSupervisorProbeEnvironment = "WINDSHARE_WINDOWS_SUPERVISOR_PROBE"
	windowsSupervisorProbeInterrupted = 23
)

func TestWindowsSupervisorProbe(t *testing.T) {
	mode := os.Getenv(windowsSupervisorProbeEnvironment)
	if mode == "" {
		t.Skip("runs only as a supervised child process")
	}
	switch mode {
	case "natural":
		os.Exit(0)
	case "deadline":
		// A shell and ping add console prompts, descendants, and network I/O to
		// a lifecycle test. This probe stays alive until the owner interrupts it.
		interrupts := make(chan os.Signal, 1)
		signal.Notify(interrupts, os.Interrupt)
		<-interrupts
		os.Exit(windowsSupervisorProbeInterrupted)
	default:
		t.Fatalf("unknown Windows supervisor probe mode %q", mode)
	}
}
