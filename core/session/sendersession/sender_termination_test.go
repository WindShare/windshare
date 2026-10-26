package sendersession

import (
	"context"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
	"time"
)

func TestSenderRuntimeLifecycleEmitsOneExplicitTerminalRoot(t *testing.T) {
	for _, test := range []struct {
		name       string
		terminate  func(*Runtime) error
		trigger    sessionobs.SenderSessionTerminalTrigger
		provenance sessionobs.SenderSessionTerminalProvenance
	}{
		{
			name: "graceful stop",
			terminate: func(runtime *Runtime) error {
				if err := runtime.BeginStop(context.Background(), "test stop"); err != nil {
					return err
				}
				return runtime.WaitStopped(context.Background())
			},
			trigger:    sessionobs.SenderSessionTerminalTriggerGracefulStop,
			provenance: sessionobs.SenderSessionTerminalProvenanceNormalStop,
		},
		{
			name: "forced close",
			terminate: func(runtime *Runtime) error {
				runtime.BeginClose()
				runtime.WaitClosed()
				return nil
			},
			trigger:    sessionobs.SenderSessionTerminalTriggerForcedClose,
			provenance: sessionobs.SenderSessionTerminalProvenanceCallerStop,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newVerticalFixture(t)
			observed := make(chan sessionobs.SenderSessionTerminated, 2)
			fixture.senderFactory.terminalObserver = newSenderTerminalObservers(
				nil,
				sessionobs.SenderSessionTerminalObserverFunc(func(event sessionobs.SenderSessionTerminated) {
					observed <- event
				}),
			)
			sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
			t.Cleanup(receiver.Close)
			t.Cleanup(sender.Close)

			if err := test.terminate(sender); err != nil {
				t.Fatalf("terminate sender: %v", err)
			}
			event := awaitSenderSessionTermination(t, observed)
			assertSenderSessionTermination(
				t,
				event,
				sender.ProtocolSessionID(),
				test.trigger,
				test.provenance,
			)
			assertNoSenderSessionTermination(t, observed)
		})
	}
}

func awaitSenderSessionTermination(
	t *testing.T,
	observed <-chan sessionobs.SenderSessionTerminated,
) sessionobs.SenderSessionTerminated {
	t.Helper()
	select {
	case event := <-observed:
		return event
	case <-time.After(time.Second):
		t.Fatal("sender session terminal root was not observed")
		return sessionobs.SenderSessionTerminated{}
	}
}

func assertNoSenderSessionTermination(
	t *testing.T,
	observed <-chan sessionobs.SenderSessionTerminated,
) {
	t.Helper()
	select {
	case event := <-observed:
		t.Fatalf("unexpected additional sender session terminal root: %+v", event)
	default:
	}
}

func assertSenderSessionTermination(
	t *testing.T,
	event sessionobs.SenderSessionTerminated,
	sessionID protocolsession.ProtocolSessionID,
	trigger sessionobs.SenderSessionTerminalTrigger,
	provenance sessionobs.SenderSessionTerminalProvenance,
) {
	t.Helper()
	if !event.Valid() || event.ProtocolSessionID != sessionID ||
		event.Trigger != trigger || event.Provenance != provenance {
		t.Fatalf(
			"sender session terminal=%+v, want session=%v trigger=%q provenance=%q",
			event,
			sessionID,
			trigger,
			provenance,
		)
	}
}
