package cli

import (
	"reflect"
	"testing"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func TestShareObservationsProjectEverySenderProducerToTypedEvents(t *testing.T) {
	emitter := &shareRecordingEmitter{}
	observations := newShareObservations(emitter)
	authority, err := clievent.NewRelayAuthority(clievent.RelayWSS, "relay.example", 443)
	if err != nil {
		t.Fatal(err)
	}
	observations.SetRelayAuthority(authority)

	observations.TraceCatalogStorage(liveshare.CatalogStorageTrace{
		Operation: liveshare.CatalogStorageCreating,
		Cause:     liveshare.CatalogStorageCauseNone,
	})
	observations.TraceRootPrefetch(liveshare.RootPrefetchTrace{
		Decision: liveshare.RootPrefetchAttemptStarted,
		Attempt:  1,
	})
	observations.TraceRelayLifecycle(relayv2.LifecycleTrace{
		LinkID:     1,
		Stage:      relayv2.LifecycleLinkClosed,
		Cause:      relayv2.LifecycleCauseNone,
		DrainCause: relayv2.LifecycleCauseNone,
	})
	observations.TraceWebRTCLifecycle(wsrtc.LifecycleTrace{
		ChannelID:  2,
		Operation:  wsrtc.LifecycleOperationChannel,
		Transition: wsrtc.LifecycleTransitionClosedClean,
		State:      framechannel.Closed,
		Terminal:   wsrtc.LifecycleTerminalNone,
		Cause:      wsrtc.LifecycleCauseNone,
	})
	observations.ObserveSenderAttempt(testShareAttemptObservation(t))
	observations.ObserveSenderTerminal(sessionruntime.SenderTerminalObservation{
		ProtocolSessionID:    testShareProtocolSessionID(t, 0x31),
		Lane:                 sessionruntime.LaneIdentity{ID: 3, Epoch: 4},
		Settled:              true,
		TransportDisposition: sessionruntime.SenderTerminalTransportAccepted,
		Outcome:              sessionruntime.SenderTerminalOutcomeDelivered,
		Decision:             sessionruntime.SenderTerminalDecisionDelivered,
	})
	observations.ObserveRelayRecovery(senderRelayRecoveryAttempt{
		attempt: 2,
		state:   senderRelayAttemptSucceeded,
	})

	wantTypes := []any{
		clievent.CatalogStorageObserved{},
		clievent.RootPrefetchObserved{},
		clievent.RelayLifecycleObserved{},
		clievent.WebRTCLifecycleObserved{},
		clievent.PeerAttemptObserved{},
		clievent.SenderTerminalObserved{},
		clievent.RelayRecovering{},
	}
	if len(emitter.events) != len(wantTypes) || emitter.lifecycleLoss != 0 {
		t.Fatalf("events = %#v, lifecycle loss = %d", emitter.events, emitter.lifecycleLoss)
	}
	for index, want := range wantTypes {
		if eventTypeName(emitter.events[index]) != eventTypeName(want) {
			t.Fatalf("event %d type = %T, want %T", index, emitter.events[index], want)
		}
	}
}

func TestShareObservationsCollapseInvalidProducerFactsWithoutProviderText(t *testing.T) {
	emitter := &shareRecordingEmitter{}
	observations := newShareObservations(emitter)
	for range 2 {
		observations.TraceRootPrefetch(liveshare.RootPrefetchTrace{
			Decision: liveshare.RootPrefetchDecision(255),
			Attempt:  1,
		})
	}
	if emitter.lifecycleLoss != 1 || len(emitter.events) != 1 {
		t.Fatalf("loss = %d events = %#v", emitter.lifecycleLoss, emitter.events)
	}
	warning, ok := emitter.events[0].(clievent.Warning)
	if !ok || warning.Failure().Code() != clievent.FailureUnexpected {
		t.Fatalf("safe projection warning = %#v", emitter.events[0])
	}
}

func TestShareCommandFailureUsesGuaranteedPublication(t *testing.T) {
	emitter := &shareRecordingEmitter{}
	emitShareKnownFailure(emitter, ExitNetwork, clievent.FailureRelayTransport)
	if len(emitter.published) != 1 || len(emitter.events) != 0 {
		t.Fatalf("published failure = %#v, observed = %#v", emitter.published, emitter.events)
	}
	if _, ok := emitter.published[0].(clievent.CommandFailed); !ok {
		t.Fatalf("published failure type = %T", emitter.published[0])
	}
}

func TestShareReadyEventsPreserveHumanMilestoneOrder(t *testing.T) {
	emitter := &shareRecordingEmitter{}
	subject, err := clievent.NewDirectorySubject(clievent.NewDisplayName("selected-root"))
	if err != nil {
		t.Fatal(err)
	}
	selected, err := clievent.NewSharingSubjectSelected(subject)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := clievent.NewRelayAuthority(clievent.RelayWSS, "relay.example", 443)
	if err != nil {
		t.Fatal(err)
	}
	connected, err := clievent.NewRelayConnected(clievent.CommandShare, authority)
	if err != nil {
		t.Fatal(err)
	}
	emitShareReady(emitter, selected, connected)
	want := []any{clievent.Ready{}, clievent.SharingSubjectSelected{}, clievent.RelayConnected{}}
	if len(emitter.published) != len(want) || len(emitter.events) != 0 {
		t.Fatalf("published ready events = %#v, observed = %#v", emitter.published, emitter.events)
	}
	for index := range want {
		if eventTypeName(emitter.published[index]) != eventTypeName(want[index]) {
			t.Fatalf("ready event %d = %T, want %T", index, emitter.published[index], want[index])
		}
	}
}

type shareRecordingEmitter struct {
	events        []clievent.Event
	published     []clievent.Event
	lifecycleLoss uint64
	progressLoss  uint64
}

func (emitter *shareRecordingEmitter) Observe(event clievent.Event) bool {
	emitter.events = append(emitter.events, event)
	return true
}

func (emitter *shareRecordingEmitter) Publish(events ...clievent.Event) bool {
	emitter.published = append(emitter.published, events...)
	return true
}

func (emitter *shareRecordingEmitter) ReportObserverLoss(lifecycle, progress uint64) bool {
	emitter.lifecycleLoss += lifecycle
	emitter.progressLoss += progress
	return true
}

func testShareAttemptObservation(t *testing.T) v2peer.SenderAttemptObservation {
	t.Helper()
	var path v2signal.PeerPathID
	var attempt v2signal.AttemptID
	for index := range path {
		path[index] = byte(index + 1)
		attempt[index] = byte(index + 17)
	}
	return v2peer.SenderAttemptObservation{
		SessionID:            testShareProtocolSessionID(t, 0x21),
		PeerPathID:           path,
		AttemptID:            attempt,
		SideSequence:         1,
		AttemptElapsedMillis: 2,
		Stage:                v2peer.SenderAttemptStarted,
	}
}

func testShareProtocolSessionID(t *testing.T, marker byte) protocolsession.ProtocolSessionID {
	t.Helper()
	raw := make([]byte, protocolsession.IdentityBytes)
	raw[len(raw)-1] = marker
	id, err := protocolsession.ProtocolSessionIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func eventTypeName(value any) string { return reflect.TypeOf(value).String() }
