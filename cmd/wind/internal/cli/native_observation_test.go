package cli

import (
	"context"
	"errors"
	"github.com/windshare/windshare/engine"
	"net/netip"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

func TestNativeObservationProjectionPreservesAttributionAndUnknowns(t *testing.T) {
	subject := nativepeer.Subject{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, AttemptID: [16]byte{3}, AttemptSequence: 4, NetworkGenerationID: 5, ICEProfileID: "ice-0123abcd", Side: nativepeer.SideReceiver}
	at := time.Unix(100, 42)
	event, err := projectNativeObservation(clievent.CommandGet, nativepeer.Observation{Subject: subject, Provider: &provider.Event{Milestone: "candidate", At: at, Candidate: &provider.CandidateFacts{Priority: 12345, TCPType: "active", Type: "host", Protocol: "tcp", Address: "opaque.local", Family: "unknown", Origin: "ordinary", Port: 123}}})
	if err != nil {
		t.Fatal(err)
	}
	facts := event.Facts()
	if facts.Session.Bytes()[0] != 1 || facts.Path.Bytes()[0] != 2 || facts.Attempt.Bytes()[0] != 3 || facts.AttemptSequence != 4 || facts.NetworkGeneration != 5 || facts.Profile != subject.ICEProfileID || facts.Side != "receiver" || facts.At != at || facts.Candidate.Address != "unknown" || facts.Candidate.Priority != 12345 || facts.Candidate.TCPType != "active" {
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

func TestShareProjectsEngineNativeObservationAndFinalLoss(t *testing.T) {
	value := nativepeer.Observation{Subject: nativepeer.Subject{Side: nativepeer.SideSender}, Provider: &provider.Event{Milestone: "provider_created"}}
	emitter := &shareRecordingEmitter{detailed: true}
	observations := newShareProjection(emitter)
	app := &App{}
	app.observeEngineShare(observations, engine.ShareObservation{Native: &value})
	app.observeEngineShare(observations, engine.ShareObservation{Loss: &engine.ShareObservationLoss{Source: engine.ShareLossNative, Dropped: 2}})
	if emitter.lifecycleLoss != 2 || len(emitter.events) != 1 {
		t.Fatalf("loss=%d events=%d", emitter.lifecycleLoss, len(emitter.events))
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
