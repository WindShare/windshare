package eventproject

import (
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/core/connectionpath"
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestEveryTURNMilestoneCrossesEngineBoundaryWithDiagnosticAttribution(t *testing.T) {
	for _, workflow := range []appobs.Workflow{appobs.WorkflowShare, appobs.WorkflowReceive} {
		for _, milestone := range connectionpath.Milestones() {
			source := nativepeer.Observation{
				Subject: nativepeer.Subject{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, Side: nativepeer.SideSender},
				TURN:    &turnpath.Event{At: time.Unix(1, 0), Subject: turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}}, Provider: "test", Account: "owner", Profile: "primary", Milestone: milestone, Reason: "diagnostic evidence"},
			}
			var input task.Event = share.Observation{Native: &source}
			if workflow == appobs.WorkflowReceive {
				source.Subject.Side = nativepeer.SideReceiver
				input = receive.NativeObserved{Value: source}
			}
			var events []task.Event
			Workflow(workflow, task.Observation{TaskID: "test:1", Event: input}, func(envelope task.Observation) bool { events = append(events, envelope.Event); return true })
			if len(events) != 1 {
				t.Fatalf("%v: events=%v", milestone, events)
			}
			normalized, ok := events[0].(appobs.NativeConnectivityObserved)
			if !ok || normalized.Validate() != nil {
				t.Fatalf("%v: %T", milestone, events[0])
			}
			facts := normalized.Facts()
			if facts.Workflow != workflow || facts.Session.Bytes()[0] != 1 || facts.Path.Bytes()[0] != 2 ||
				facts.TURN.Milestone != milestone || facts.TURN.Reason != "diagnostic evidence" || facts.TURN.Provider != "test" {
				t.Fatalf("%v lost diagnostic facts: %+v", milestone, facts)
			}
		}
	}
}

type futureWorkflowEvent struct{}

func (futureWorkflowEvent) EngineEvent() {}

func TestWorkflowRejectsUnknownOrMalformedSourceWithoutPublishingRawTypes(t *testing.T) {
	for _, source := range []task.Event{
		futureWorkflowEvent{},
		share.Observation{},
		receive.RelayObserved{Value: relayv2.LifecycleTrace{LinkID: 1, Stage: relayv2.LifecycleSendAdmitted}},
		share.Observation{Native: &nativepeer.Observation{TURN: &turnpath.Event{Milestone: 255}}},
	} {
		count := 0
		Workflow(appobs.WorkflowReceive, task.Observation{TaskID: "test:1", Event: source}, func(envelope task.Observation) bool {
			event := envelope.Event
			count++
			loss, ok := event.(appobs.ObserverLossObserved)
			if !ok || loss.Validate() != nil || loss.Count() != 1 {
				t.Fatalf("raw or invalid event escaped: %T %+v", event, event)
			}
			if _, unknown := source.(futureWorkflowEvent); unknown && loss.Category() != appobs.ObserverLossEngineObservation {
				t.Fatal("engine rejection was attributed to a client adapter")
			}
			return true
		})
		if count != 1 {
			t.Fatalf("rejection count=%d", count)
		}
	}
}

func TestWorkflowPreservesPublisherBackpressureWithoutChangingSource(t *testing.T) {
	source := receive.Warning{Code: receive.FailurePeerNegotiation}
	if Workflow(appobs.WorkflowReceive, task.Observation{TaskID: "test:1", Event: source}, func(task.Observation) bool { return false }) {
		t.Fatal("bounded publisher rejection was lost")
	}
}
