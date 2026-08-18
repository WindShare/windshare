package cli

import (
	"context"
	"os"
	"testing"
	"time"
)

const interruptTestTimeout = time.Second

func TestRunCLIWithInterruptEscalationFirstInterruptCancels(t *testing.T) {
	interrupts := make(chan os.Signal, interruptSignalBuffer)
	forced := make(chan int, 1)
	result := make(chan int, 1)
	started := make(chan struct{})
	go func() {
		result <- runCLIWithInterruptEscalation(
			interrupts,
			func(code int) { forced <- code },
			func(ctx context.Context) int {
				close(started)
				<-ctx.Done()
				return ExitFailure
			},
		)
	}()

	awaitInterruptTestSignal(t, started, "CLI start")
	interrupts <- os.Interrupt
	select {
	case code := <-result:
		if code != ExitFailure {
			t.Fatalf("exit=%d want=%d", code, ExitFailure)
		}
	case <-time.After(interruptTestTimeout):
		t.Fatal("first interrupt did not cancel the CLI")
	}
	select {
	case code := <-forced:
		t.Fatalf("first interrupt forced process exit %d", code)
	default:
	}
}

func TestRunCLIWithInterruptEscalationSecondInterruptForcesCrashCut(t *testing.T) {
	interrupts := make(chan os.Signal, interruptSignalBuffer)
	forced := make(chan int, 1)
	result := make(chan int, 1)
	started := make(chan struct{})
	canceled := make(chan struct{})
	releaseSettlement := make(chan struct{})
	go func() {
		result <- runCLIWithInterruptEscalation(
			interrupts,
			func(code int) { forced <- code },
			func(ctx context.Context) int {
				close(started)
				<-ctx.Done()
				close(canceled)
				<-releaseSettlement
				return ExitFailure
			},
		)
	}()

	awaitInterruptTestSignal(t, started, "CLI start")
	interrupts <- os.Interrupt
	awaitInterruptTestSignal(t, canceled, "first-interrupt cancellation")
	interrupts <- os.Interrupt
	select {
	case code := <-forced:
		if code != forcedInterruptExitCode {
			t.Fatalf("forced exit=%d want=%d", code, forcedInterruptExitCode)
		}
	case <-time.After(interruptTestTimeout):
		t.Fatal("second interrupt did not force the process crash cut")
	}
	close(releaseSettlement)
	select {
	case <-result:
	case <-time.After(interruptTestTimeout):
		t.Fatal("interrupt controller did not join after injected exit returned")
	}
}

func TestRunCLIWithInterruptEscalationCompletionRejectsLateForceExit(t *testing.T) {
	interrupts := make(chan os.Signal, interruptSignalBuffer)
	forced := make(chan int, 1)
	result := make(chan int, 1)
	release := make(chan struct{})
	go func() {
		result <- runCLIWithInterruptEscalation(
			interrupts,
			func(code int) { forced <- code },
			func(context.Context) int {
				<-release
				return ExitOK
			},
		)
	}()
	close(release)
	select {
	case code := <-result:
		if code != ExitOK {
			t.Fatalf("exit=%d want=%d", code, ExitOK)
		}
	case <-time.After(interruptTestTimeout):
		t.Fatal("CLI did not complete")
	}
	interrupts <- os.Interrupt
	interrupts <- os.Interrupt
	select {
	case code := <-forced:
		t.Fatalf("completed CLI accepted a late forced exit %d", code)
	default:
	}
}

func awaitInterruptTestSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(interruptTestTimeout):
		t.Fatalf("timed out waiting for %s", label)
	}
}
