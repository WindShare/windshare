package eventproject

import (
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/session/protocolsession"

	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type futureWorkflowEvent struct{}

func (futureWorkflowEvent) EngineEvent() {}

func TestWorkflowRejectsMalformedProductsWithoutPublishingSourceTypes(t *testing.T) {
	for _, source := range []task.Event{
		futureWorkflowEvent{}, share.Observation{}, receive.RelayConnected{},
		receive.ContentPathObserved{}, receive.LaneAdopted{},
	} {
		envelope := task.Observation{TaskID: "application:42", At: time.Unix(10, 0), Event: source}
		var published []task.Observation
		if !Workflow(appobs.WorkflowReceive, envelope, func(value task.Observation) bool {
			published = append(published, value)
			return true
		}) || len(published) != 1 {
			t.Fatalf("%T: publication = %+v", source, published)
		}
		if _, ok := published[0].Event.(appobs.ObserverLossObserved); !ok ||
			published[0].TaskID != envelope.TaskID || published[0].At != envelope.At {
			t.Fatalf("%T: lost rejection or envelope identity: %+v", source, published[0])
		}
	}
}

func TestWorkflowRetainsConnectionSummaryAndPublisherBackpressure(t *testing.T) {
	endpoint, err := v2.NormalizeRelayEndpoint("wss://relay.example")
	if err != nil {
		t.Fatal(err)
	}
	session := protocolsession.ProtocolSessionID{1}
	for _, source := range []task.Event{
		receive.RelayConnected{Endpoint: endpoint},
		receive.ContentPathObserved{Path: receive.ContentPathDirectAndRelay},
		receive.FallbackObserved{Code: receive.FailurePeerNegotiation},
		receive.LaneAdopted{Session: session, Lane: sessioncontract.LaneIdentity{ID: 7, Epoch: 2}},
		receive.Warning{Code: receive.FailurePeerNegotiation},
	} {
		var published task.Event
		accepted := Workflow(appobs.WorkflowReceive, task.Observation{Event: source}, func(value task.Observation) bool {
			published = value.Event
			return false
		})
		if accepted {
			t.Fatalf("%T: rejected publication reported accepted", source)
		}
		event, ok := published.(appobs.Event)
		if !ok || event.Validate() != nil {
			t.Fatalf("%T: invalid product %T", source, published)
		}
		switch value := published.(type) {
		case appobs.RelayConnected:
			want, _ := RelayOrigin(endpoint)
			if value.Origin() != want {
				t.Fatal("relay origin changed")
			}
		case appobs.ContentPathSelected:
			if value.Path() != appobs.ContentPathDirectAndRelay {
				t.Fatal("mixed path lost")
			}
		case appobs.Fallback:
			if value.From() != appobs.TransportWebRTC || value.To() != appobs.TransportRelay {
				t.Fatal("fallback direction changed")
			}
		case appobs.LaneAdopted:
			want, _ := LaneIdentity(sessioncontract.LaneIdentity{ID: 7, Epoch: 2})
			if value.Lane() != want {
				t.Fatal("lane incarnation changed")
			}
		case appobs.Warning:
		default:
			t.Fatalf("%T: unexpected product %T", source, published)
		}
	}
}

func TestWorkflowRecoveryRetainsUserVisibleWaitingAndRestoration(t *testing.T) {
	for _, phase := range []relayset.ReceiverRecoveryPhase{
		relayset.ReceiverRecoveryConnecting, relayset.ReceiverRecoveryWaiting,
		relayset.ReceiverRecoveryRetrying, relayset.ReceiverRecoveryConnected, relayset.ReceiverRecoveryTerminal,
	} {
		source := relayset.ReceiverRecoveryObservation{
			Endpoint: "wss://relay.example", Attempt: 2, Phase: phase,
			ConnectionGeneration: 3, ProtocolSessionID: [16]byte{1}, Delay: time.Second,
		}
		var event appobs.RelayRecovering
		accepted := Workflow(appobs.WorkflowReceive, task.Observation{Event: receive.RecoveryObserved{Value: source}}, func(value task.Observation) bool {
			var ok bool
			event, ok = value.Event.(appobs.RelayRecovering)
			if !ok {
				t.Fatalf("recovery became %T", value.Event)
			}
			return true
		})
		details, hasDetails := event.Details()
		if !accepted || event.Validate() != nil || !hasDetails || event.Attempt() != 2 ||
			details.Generation != 3 || details.NextDelay != time.Second || !details.ProtocolSessionID.Valid() ||
			details.Slow != (phase == relayset.ReceiverRecoveryWaiting) ||
			details.Terminal != (phase == relayset.ReceiverRecoveryTerminal) {
			t.Fatalf("phase %v: recovery details = %+v", phase, details)
		}
	}
	firstConnection := receive.RecoveryObserved{Value: relayset.ReceiverRecoveryObservation{
		Endpoint: "wss://relay.example", Attempt: 1, Phase: relayset.ReceiverRecoveryConnected,
	}}
	if !Workflow(appobs.WorkflowReceive, task.Observation{Event: firstConnection}, func(task.Observation) bool {
		t.Fatal("initial connection incorrectly announced as recovered")
		return false
	}) {
		t.Fatal("suppressed initial recovery rejected")
	}
}
