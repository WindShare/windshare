package cli

import (
	"context"
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
	emitter := &shareRecordingEmitter{detailed: true}
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
		LinkID: 1, OperationID: 1,
		Stage: relayv2.LifecycleLinkClosed, RetirementSource: relayv2.LifecycleRetirementLocalClose,
		Cause: relayv2.LifecycleCauseNone, DrainCause: relayv2.LifecycleCauseNone,
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
	emitter := &shareRecordingEmitter{detailed: true}
	observations := newShareObservations(emitter)
	for range 2 {
		observations.TraceRootPrefetch(liveshare.RootPrefetchTrace{
			Decision: liveshare.RootPrefetchDecision(255),
			Attempt:  1,
		})
	}
	if emitter.lifecycleLoss != 2 || len(emitter.events) != 0 {
		t.Fatalf("loss = %d events = %#v", emitter.lifecycleLoss, emitter.events)
	}
}

func TestShareRelayDropSummaryAndCompletionUseOneCumulativeSource(t *testing.T) {
	emitter := &shareRecordingEmitter{detailed: true}
	observations := newShareObservations(emitter)
	dropped := relayv2.LifecycleTrace{
		LinkID: 4, Stage: relayv2.LifecycleTraceDropped,
		RetirementSource: relayv2.LifecycleRetirementNone,
		Cause:            relayv2.LifecycleCauseNone, DrainCause: relayv2.LifecycleCauseNone,
		Dropped: 6,
	}
	observations.TraceRelayLifecycle(dropped)
	observations.reportRelayCompletion(relayv2.LifecycleObservationCompletion{
		Drained: true, Loss: relayv2.LifecycleObservationLoss{QueueOverflow: 6},
	})
	observations.TraceRelayLifecycle(dropped)

	if len(emitter.events) != 2 {
		t.Fatalf("safe lifecycle events=%#v", emitter.events)
	}
	for _, event := range emitter.events {
		value, ok := event.(clievent.RelayLifecycleObserved)
		if !ok || value.Stage() != clievent.RelayTraceDropped || value.Dropped() != 6 {
			t.Fatalf("drop event=%#v", event)
		}
	}
	if emitter.lifecycleLoss != 6 {
		t.Fatalf("cumulative relay loss=%d", emitter.lifecycleLoss)
	}
}

func TestShareContextObserversCannotCommitAfterAuthorityRevocation(t *testing.T) {
	emitter := &shareRecordingEmitter{detailed: true}
	observations := newShareObservations(emitter)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	observations.TraceWebRTCLifecycleContext(ctx, wsrtc.LifecycleTrace{
		ChannelID: 1, Operation: wsrtc.LifecycleOperationChannel,
		Transition: wsrtc.LifecycleTransitionClosedClean,
		State:      framechannel.Closed, Terminal: wsrtc.LifecycleTerminalNone, Cause: wsrtc.LifecycleCauseNone,
	})
	observations.ObserveSenderAttemptContext(ctx, testShareAttemptObservation(t))

	// Completion revocation is authoritative even when the producer callback's
	// context has not yet observed cancellation.
	observations.webRTCGate.revoke()
	observations.peerGate.revoke()
	observations.TraceWebRTCLifecycleContext(context.Background(), wsrtc.LifecycleTrace{
		ChannelID: 2, Operation: wsrtc.LifecycleOperationChannel,
		Transition: wsrtc.LifecycleTransitionClosedClean,
		State:      framechannel.Closed, Terminal: wsrtc.LifecycleTerminalNone, Cause: wsrtc.LifecycleCauseNone,
	})
	observations.ObserveSenderAttemptContext(context.Background(), testShareAttemptObservation(t))
	if len(emitter.events) != 0 || emitter.lifecycleLoss != 0 {
		t.Fatalf("revoked callbacks committed events=%#v loss=%d", emitter.events, emitter.lifecycleLoss)
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
	detailed      bool
}

func (emitter *shareRecordingEmitter) Observe(event clievent.Event) bool {
	emitter.events = append(emitter.events, event)
	return true
}

func (emitter *shareRecordingEmitter) Publish(events ...clievent.Event) bool {
	emitter.published = append(emitter.published, events...)
	return true
}

func (emitter *shareRecordingEmitter) ReportObserverLoss(_ clievent.ObserverLossCategory, _ clievent.ObserverLossReason, count uint64) bool {
	emitter.lifecycleLoss += count
	return true
}

func (emitter *shareRecordingEmitter) detailedDiagnosticsEnabled() bool { return emitter.detailed }

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
