package cli

import (
	"context"
	"os"
	"syscall"
	"testing"
	"testing/synctest"
)

func TestServiceStopNotificationsDoNotForceSettlement(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		signals []os.Signal
		force   bool
	}{
		{"term then hangup", []os.Signal{syscall.SIGTERM, syscall.SIGHUP}, false},
		{"hangup then term", []os.Signal{syscall.SIGHUP, syscall.SIGTERM}, false},
		{"repeated term", []os.Signal{syscall.SIGTERM, syscall.SIGTERM}, false},
		{"repeated hangup", []os.Signal{syscall.SIGHUP, syscall.SIGHUP}, false},
		{"interrupt then service stop", []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}, false},
		{"service stop then one interrupt", []os.Signal{syscall.SIGTERM, os.Interrupt, syscall.SIGHUP}, false},
		{"two interrupts after service stop", []os.Signal{syscall.SIGTERM, os.Interrupt, os.Interrupt}, true},
		{"two interrupts around service stop", []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, os.Interrupt}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				signals := make(chan os.Signal, interruptSignalBuffer)
				forced := make(chan int, 1)
				done := make(chan int, 1)
				cancelled := make(chan struct{})
				settlement := make(chan struct{})
				go func() {
					done <- runCLIWithInterruptEscalation(signals, func(code int) { forced <- code },
						func(ctx context.Context) int {
							<-ctx.Done()
							close(cancelled)
							<-settlement
							return ExitOK
						})
				}()
				defer func() {
					close(settlement)
					if code := <-done; code != ExitOK {
						t.Errorf("settlement result=%d", code)
					}
				}()
				for _, request := range scenario.signals {
					signals <- request
					synctest.Wait()
				}
				select {
				case <-cancelled:
				default:
					t.Fatal("first stop did not cancel the command")
				}
				select {
				case code := <-forced:
					if !scenario.force || code != forcedInterruptExitCode {
						t.Fatalf("unexpected forced exit=%d wantForce=%v", code, scenario.force)
					}
				default:
					if scenario.force {
						t.Fatal("second Ctrl-C did not force exit")
					}
				}
			})
		})
	}
}
