//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestNotifyTerminationSignalsDeliversServiceStopRequests(t *testing.T) {
	for _, request := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(request.String(), func(t *testing.T) {
			interrupts := make(chan os.Signal, interruptSignalBuffer)
			stop := notifyTerminationSignals(interrupts, func(os.Signal) bool { return false })
			defer stop()

			if err := syscall.Kill(os.Getpid(), request); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-interrupts:
				if got != request {
					t.Fatalf("signal=%v want=%v", got, request)
				}
			case <-time.After(interruptTestTimeout):
				t.Fatalf("%v was not routed to graceful shutdown", request)
			}
		})
	}
}

func TestNotifyTerminationSignalsKeepsInheritedHangupIgnore(t *testing.T) {
	signal.Ignore(syscall.SIGHUP)
	t.Cleanup(func() { signal.Reset(syscall.SIGHUP) })
	interrupts := make(chan os.Signal, interruptSignalBuffer)
	stop := notifyTerminationSignals(interrupts, signal.Ignored)
	defer stop()

	if !signal.Ignored(syscall.SIGHUP) {
		t.Fatal("registration replaced an inherited SIGHUP ignore, so nohup no longer survives the terminal")
	}
	// An ignored SIGHUP must stay a no-op, while Ctrl-C and SIGTERM still stop.
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-interrupts:
		if got != syscall.SIGTERM {
			t.Fatalf("first routed signal=%v want SIGTERM", got)
		}
	case <-time.After(interruptTestTimeout):
		t.Fatal("SIGTERM was not routed to graceful shutdown")
	}
}
