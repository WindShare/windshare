package networkcommand

import (
	"context"
	"errors"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/internal/platformsetup"
)

type Checker interface {
	Check(context.Context) platformsetup.Report
}

type Observer interface{ Observe(clievent.Event) bool }

// StartCheck never waits for platform I/O. The command owns cancellation and joins
// the check before closing its event stream, so diagnostics cannot outlive output.
func StartCheck(parent context.Context, checker Checker, observer Observer, command clievent.Command) func() {
	if checker == nil {
		return func() {}
	}
	ctx, cancel := context.WithTimeout(parent, platformsetup.CheckTimeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		report := checker.Check(ctx)
		if parent.Err() != nil || errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		suggested := false
		for _, finding := range report.Findings {
			suggest := finding.Repairable() && report.Choice != platformsetup.Skip && report.PreferenceError == "" && !suggested
			event, err := clievent.NewNetworkPolicyObserved(command, report.Choice, finding, suggest)
			if err == nil {
				observer.Observe(event)
			}
			suggested = suggested || suggest
		}
	}()
	return func() { cancel(); <-done }
}
