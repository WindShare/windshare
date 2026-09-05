package cli

import (
	"bytes"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/internal/platformsetup"
)

func TestCommandRuntimeReportsInjectedInstallationDecision(t *testing.T) {
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	app := &App{
		Stderr:              bytes.NewBuffer(nil),
		platformSetupStatus: &platformsetup.Status{State: platformsetup.Declined, Reason: "user-skipped"},
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.Close()
	events := recorder.recorded()
	if len(events) != 1 {
		t.Fatalf("installation facts: %v", events)
	}
	event, ok := events[0].(clievent.PlatformSetupObserved)
	if !ok || event.State() != platformsetup.Declined || event.Reason() != "user-skipped" {
		t.Fatal(events)
	}
}
