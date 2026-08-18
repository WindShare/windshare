package cli

import (
	"context"
	"os"
	"sync"
)

const (
	interruptSignalBuffer = 2
	// Shells conventionally report SIGINT termination as 128 + signal 2.
	forcedInterruptExitCode = 130
)

// runCLIWithInterruptEscalation gives the first interrupt to the durable
// settlement path, but leaves the operator a second interrupt that cannot be
// delayed by stuck I/O. The second path deliberately bypasses defers so native
// output recovery observes the same boundary as an abrupt process stop.
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
	interrupted := false

	go func() {
		defer close(monitorDone)
		for {
			select {
			case <-finished:
				return
			case _, ok := <-interrupts:
				if !ok {
					return
				}
				stateMu.Lock()
				if completed {
					stateMu.Unlock()
					return
				}
				if interrupted {
					// Serialize this decision with normal completion. If completion
					// wins the lock, no late signal can turn success into a forced exit.
					stateMu.Unlock()
					forceExit(forcedInterruptExitCode)
					return
				}
				interrupted = true
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
