package cli

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

type testNativeObservationSource struct {
	producer observationstream.Producer[nativepeer.Observation]
	stream   observationstream.Consumer[nativepeer.Observation]
}

func (source *testNativeObservationSource) Observations() <-chan nativepeer.Observation {
	return source.stream
}
func (source *testNativeObservationSource) CompleteObservations() observationstream.Completion {
	return source.producer.Complete()
}

func TestNativeObservationBridgeDrainsBoundedPrefixAndAccountsCapacity(t *testing.T) {
	producer, stream, err := observationstream.New[nativepeer.Observation](1)
	if err != nil {
		t.Fatal(err)
	}
	value := nativepeer.Observation{Subject: nativepeer.Subject{Side: nativepeer.SideSender}, Provider: &provider.Event{Milestone: "provider_created"}}
	producer.TryPublish(value)
	producer.TryPublish(value)
	producer.TryPublish(value)
	source := &testNativeObservationSource{producer: producer, stream: stream}
	emitter := &shareRecordingEmitter{}
	reader := startNativeObservation(source, clievent.CommandShare, emitter)
	completion, status := reader.complete(context.Background())
	if completion.Enqueued != 1 || completion.CapacityDropped != 2 || !status.Joined || status.Forwarded != 1 {
		t.Fatalf("completion=%+v status=%+v", completion, status)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("events=%d", len(emitter.events))
	}
	if _, ok := emitter.events[0].(clievent.NativeConnectivityObserved); !ok {
		t.Fatalf("event=%T", emitter.events[0])
	}
	next, nextStatus := reader.complete(context.Background())
	if next != completion || nextStatus != status {
		t.Fatal("completion not idempotent")
	}
	if producer.TryPublish(value) {
		t.Fatal("post-cut publication accepted")
	}
	if c, s := (nativeObservationReader{}).complete(context.Background()); c.Enqueued != 0 || !s.Joined {
		t.Fatal("disabled source not empty")
	}
	if startNativeObservation(nil, clievent.CommandGet, emitter).reader != nil {
		t.Fatal("nil source enabled")
	}
	if startNativeObservation(source, clievent.CommandGet, nil).reader != nil {
		t.Fatal("nil sink enabled")
	}
}

func TestNativeObservationBridgeRejectsOpenProviderFacts(t *testing.T) {
	producer, stream, _ := observationstream.New[nativepeer.Observation](1)
	producer.TryPublish(nativepeer.Observation{Provider: &provider.Event{Milestone: "raw-error-token"}})
	emitter := &shareRecordingEmitter{}
	reader := startNativeObservation(&testNativeObservationSource{producer: producer, stream: stream}, clievent.CommandGet, emitter)
	_, status := reader.complete(context.Background())
	if !status.Joined || len(emitter.events) != 0 || emitter.lifecycleLoss != 1 {
		t.Fatalf("status=%+v emitter=%+v", status, emitter)
	}
}

func TestNativeObservationProjectionPreservesAttributionAndUnknowns(t *testing.T) {
	subject := nativepeer.Subject{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, AttemptID: [16]byte{3}, AttemptSequence: 4, NetworkGenerationID: 5, ICEProfileID: "ice-0123abcd", Side: nativepeer.SideReceiver}
	at := time.Unix(100, 42)
	event, err := projectNativeObservation(clievent.CommandGet, nativepeer.Observation{Subject: subject, Provider: &provider.Event{Milestone: "candidate", At: at, Candidate: &provider.CandidateFacts{Type: "host", Protocol: "udp", Address: "opaque.local", Family: "unknown", Origin: "ordinary", Port: 123}}})
	if err != nil {
		t.Fatal(err)
	}
	facts := event.Facts()
	if facts.Session.Bytes()[0] != 1 || facts.Path.Bytes()[0] != 2 || facts.Attempt.Bytes()[0] != 3 || facts.AttemptSequence != 4 || facts.NetworkGeneration != 5 || facts.Profile != subject.ICEProfileID || facts.Side != "receiver" || facts.At != at || facts.Candidate.Address != "unknown" {
		t.Fatalf("facts=%+v", facts)
	}
	for _, value := range []nativepeer.Observation{
		{Subject: subject, Provider: &provider.Event{Milestone: "selected_pair", Pair: &provider.PairFacts{LocalType: "host", RemoteType: "srflx", Protocol: "tcp", LocalAddress: "::1", RemoteAddress: "127.0.0.1", RoundTripTime: time.Millisecond}}},
		{Subject: subject, Provider: &provider.Event{Milestone: "ice", State: "connected"}},
		{Subject: subject, Provider: &provider.Event{Milestone: "tcp_unavailable", State: "credential-canary"}},
		{Reachability: &reachability.Event{Kind: "lease-ready", Endpoint: reachability.Endpoint{Protocol: reachability.UDP, Local: netip.MustParseAddrPort("127.0.0.1:123")}}},
		{Reachability: &reachability.Event{Kind: "lease-failed", Endpoint: reachability.Endpoint{Protocol: reachability.TCP}, Error: reachability.ErrUnavailable}},
		{Reachability: &reachability.Event{Kind: "gateway-unavailable"}},
		{Subject: subject, Lifecycle: &nativepeer.LifecycleFacts{Kind: nativepeer.DemandChanged, At: at, Content: true, Direct: true}},
	} {
		if _, err := projectNativeObservation(clievent.CommandGet, value); err != nil {
			t.Fatalf("value=%+v error=%v", value, err)
		}
	}
	if _, err := projectNativeObservation(clievent.CommandGet, nativepeer.Observation{}); err == nil {
		t.Fatal("empty union accepted")
	}
	if _, err := projectNativeObservation(clievent.CommandGet, nativepeer.Observation{Provider: &provider.Event{}, Reachability: &reachability.Event{}}); err == nil {
		t.Fatal("ambiguous union accepted")
	}
	cases := []struct {
		err  error
		want string
	}{{nil, "none"}, {context.Canceled, "canceled"}, {context.DeadlineExceeded, "deadline"}, {reachability.ErrUnavailable, "unavailable"}, {reachability.ErrCapacity, "capacity"}, {reachability.ErrInvalidResponse, "invalid_response"}, {reachability.ErrClosed, "closed"}, {reachability.ErrLeaseLost, "lease_lost"}, {errors.New("credential-canary"), "unknown"}}
	for _, test := range cases {
		if got := nativeReachabilityReason(test.err); got != test.want {
			t.Fatalf("reason=%s want=%s", got, test.want)
		}
	}
}

func TestNativeObservationRegistrationRespectsCommandVisibilityAndClose(t *testing.T) {
	for _, detailed := range []bool{false, true} {
		runtime, _ := newGetReportingRuntime(t, false, detailed)
		observation := newGetObservation(runtime)
		native := nativepeer.New(nativepeer.Config{Side: nativepeer.SideReceiver, ObservationCapacity: nativepeer.DefaultObservationCapacity})
		observation.registerNative(native)
		if (observation.state.native.reader != nil) != detailed {
			t.Fatal("visibility did not control reader")
		}
		if err := native.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		observation.complete(context.Background())
		observation.complete(context.Background())
		runtime.Close()
	}
	(getObservation{}).registerNative(nil)
}

func TestShareNativeObserverLossIsReportedOnceAtFinalCut(t *testing.T) {
	producer, stream, _ := observationstream.New[nativepeer.Observation](1)
	value := nativepeer.Observation{Subject: nativepeer.Subject{Side: nativepeer.SideSender}, Provider: &provider.Event{Milestone: "provider_created"}}
	producer.TryPublish(value)
	producer.TryPublish(value)
	producer.TryPublish(value)
	emitter := &shareRecordingEmitter{detailed: true}
	observations := newShareObservations(emitter)
	observations.nativeReader = startNativeObservation(&testNativeObservationSource{producer: producer, stream: stream}, clievent.CommandShare, emitter)
	observations.completeWithin()
	observations.completeWithin()
	if emitter.lifecycleLoss != 2 || len(emitter.events) != 1 {
		t.Fatalf("loss=%d events=%d", emitter.lifecycleLoss, len(emitter.events))
	}
}

func TestSenderNativeQueueTracksDetailedCommandOwnership(t *testing.T) {
	for _, detailed := range []bool{false, true} {
		emitter := &shareRecordingEmitter{detailed: detailed}
		observations := newShareObservations(emitter)
		factory, err := (&App{}).newSenderPeerFactory(observations, nil)
		if err != nil {
			t.Fatal(err)
		}
		if (factory.NativeConnectivity().Observations() != nil) != detailed || (observations.nativeReader.reader != nil) != detailed {
			t.Fatal("sender native queue visibility differs from owner reader")
		}
		observations.completeWithin()
		observations.completeWithin()
	}
}

func TestNativeProcessAdmissionProjectionKeepsQueueAndAllowanceFacts(t *testing.T) {
	at := time.Unix(100, 0)
	for _, kind := range []nativepeer.AdmissionKind{nativepeer.AdmissionQueued, nativepeer.AdmissionGranted, nativepeer.AdmissionReleased, nativepeer.AdmissionRejected} {
		value := nativepeer.Observation{Admission: &nativepeer.AdmissionFacts{Kind: kind, At: at, Wait: time.Second, Active: 2, Queued: 3, StartsRemaining: 1.5, STUNRemaining: 0.25, ActiveTimeRemaining: 85 * time.Second}}
		event, err := projectNativeObservation(clievent.CommandGet, value)
		if err != nil {
			t.Fatal(err)
		}
		facts := event.Facts()
		if facts.Kind != "admission_"+string(kind) || facts.At != at || facts.Admission.Active != 2 || facts.Admission.Queued != 3 || facts.Admission.Wait != time.Second || facts.Admission.STUNRemaining != 0.25 || facts.Admission.ActiveTimeRemaining != 85*time.Second {
			t.Fatalf("facts=%+v", facts)
		}
		value.Admission.Active = -1
		if _, err := projectNativeObservation(clievent.CommandGet, value); err == nil {
			t.Fatal("negative active count accepted")
		}
	}
}
