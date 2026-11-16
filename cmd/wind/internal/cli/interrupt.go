package cli

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

const (
	interruptSignalBuffer = 2
	// Shells conventionally report SIGINT termination as 128 + signal 2.
	forcedInterruptExitCode = 130
)

// notifyTerminationSignals routes every ordinary request to end the process to
// ch: Ctrl-C, SIGTERM from service managers, containers, and kill, and SIGHUP
// from a closing terminal or SSH session. Unhandled, the latter two end the
// process at once, so a sender never publishes its stop marker and receivers
// are told to retry a link that will never come back.
//
// A SIGHUP that was already ignored when the process started (nohup, disown
// in some shells) stays ignored, because registering a handler would silently
// override that explicit request to outlive the terminal.
func notifyTerminationSignals(ch chan<- os.Signal, ignored func(os.Signal) bool) (stop func()) {
	signals := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !ignored(syscall.SIGHUP) {
		signals = append(signals, syscall.SIGHUP)
	}
	signal.Notify(ch, signals...)
	return func() { signal.Stop(ch) }
}

// runCLIWithInterruptEscalation gives termination requests to durable settlement.
// Service-manager notifications remain idempotent: SIGTERM followed by SIGHUP
// can be one stop request, not an instruction to abandon cleanup. Only a second
// Ctrl-C forces exit without defers, so an operator can still escape stuck I/O.
func runCLIWithInterruptEscalation(
	interrupts <-chan os.Signal,
	forceExit func(int),
	run func(context.Context) int,
) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	finished := make(chan struct{})
	monitorDone := make(chan struct{})
	var stateMu sync.Mutex
	completed := false
	interruptReceived := false

	go func() {
		defer close(monitorDone)
		for {
			select {
			case <-finished:
				return
			case request, ok := <-interrupts:
				if !ok {
					return
				}
				stateMu.Lock()
				if completed {
					stateMu.Unlock()
					return
				}
				if request == os.Interrupt && interruptReceived {
					// Serialize this decision with normal completion. If completion
					// wins the lock, no late signal can turn success into a forced exit.
					stateMu.Unlock()
					forceExit(forcedInterruptExitCode)
					return
				}
				interruptReceived = interruptReceived || request == os.Interrupt
				stateMu.Unlock()
				cancel()
			}
		}
	}()

	code := run(ctx)
	stateMu.Lock()
	completed = true
	stateMu.Unlock()
	close(finished)
	<-monitorDone
	return code
}
