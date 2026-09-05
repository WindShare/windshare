package runtrace

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestNativeConnectivityExactPayloadAndCorrelation(t *testing.T) {
	session, _ := clievent.NewProtocolSessionID(append([]byte{1}, make([]byte, 15)...))
	path, _ := clievent.NewPeerPathID(append([]byte{2}, make([]byte, 15)...))
	attempt, _ := clievent.NewPeerAttemptID(append([]byte{3}, make([]byte, 15)...))
	spec := clievent.NativeConnectivitySpec{Command: clievent.CommandGet, Session: session, Path: path, Attempt: attempt, AttemptSequence: 9007199254740993, NetworkGeneration: 9007199254740994, Profile: "ice-0123abcd", Side: "receiver", Kind: "candidate", State: "unknown", At: time.Unix(0, 0), Candidate: &clievent.NativeCandidateFacts{Type: "srflx", Protocol: "udp", Address: "8.8.8.8", Port: 123, Family: "ipv4", Origin: "ordinary"}}
	event, err := clievent.NewNativeConnectivityObserved(spec)
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeV3(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(0, 0)}, event)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(record.Payload)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"candidate","state":"unknown","side":"receiver","attempt_sequence":"9007199254740993","network_generation_id":"9007199254740994","ice_profile_id":"ice-0123abcd","observed_at":"1970-01-01T00:00:00Z","candidate":{"type":"srflx","protocol":"udp","address":"8.8.8.8","port":123,"family":"ipv4","origin":"ordinary","interface_class":"unknown","stun_endpoint":"unknown","stun_rtt_ms":"unknown","policy_decision":"unknown"}}`
	if string(raw) != want {
		t.Fatalf("payload=%s", raw)
	}
	if record.Event != "native_connectivity" || record.Correlation.ProtocolSessionID != "AQAAAAAAAAAAAAAAAAAAAA" || record.Correlation.PeerPathID != "AgAAAAAAAAAAAAAAAAAAAA" || record.Correlation.PeerAttemptID != "AwAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("record=%+v", record)
	}
}

func TestNativeConnectivityUnknownsRemainDistinctFromPairMeasurements(t *testing.T) {
	for _, rtt := range []time.Duration{0, 12500 * time.Microsecond} {
		spec := clievent.NativeConnectivitySpec{Command: clievent.CommandShare, Side: "sender", Kind: "selected_pair", State: "unknown", Pair: &clievent.NativePairFacts{LocalType: "host", RemoteType: "relay", Protocol: "tcp", LocalAddress: "::1", RemoteAddress: "unknown", PairRTT: rtt}}
		event, err := clievent.NewNativeConnectivityObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		record, err := encodeV3(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(0, 0)}, event)
		if err != nil {
			t.Fatal(err)
		}
		payload := record.Payload.(nativeConnectivityPayloadV3)
		if record.Correlation != nil || payload.Profile != "unknown" || payload.NetworkGeneration != "unknown" || payload.AttemptSequence != "unknown" || payload.ObservedAt != "unknown" {
			t.Fatalf("invented attribution=%+v", payload)
		}
		want := "unknown"
		if rtt > 0 {
			want = "12.500"
		}
		if payload.Pair.PairRTT != want || payload.Pair.LocalFamily != "ipv6" || payload.Pair.RemoteFamily != "unknown" || payload.Pair.Lifetime != "unknown" || payload.Pair.SwitchReason != "unknown" {
			t.Fatalf("pair=%+v", payload.Pair)
		}
		raw, _ := json.Marshal(payload)
		if strings.Contains(string(raw), "stun_rtt") {
			t.Fatal("pair RTT became a STUN measurement")
		}
	}
	for _, spec := range []clievent.NativeConnectivitySpec{
		{Command: clievent.CommandGet, Side: "unknown", Kind: "lease-ready", State: "unknown", Reachability: &clievent.NativeReachabilityFacts{Protocol: "udp", Reason: "none", Local: netip.MustParseAddrPort("127.0.0.1:123"), ServerEpoch: 42, ServerRestarted: true}},
		{Command: clievent.CommandGet, Side: "receiver", Kind: "network_changed", State: "unknown", Lifecycle: &clievent.NativeLifecycleFacts{Content: true, Direct: false, PreviousGeneration: 99}},
	} {
		event, err := clievent.NewNativeConnectivityObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		record, err := encodeV3(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(0, 0)}, event)
		if err != nil {
			t.Fatal(err)
		}
		payload := record.Payload.(nativeConnectivityPayloadV3)
		if r := payload.Reachability; r != nil && (r.Local != "127.0.0.1:123" || r.Remote != "unknown" || r.ServerEpoch != 42 || !r.ServerRestarted) {
			t.Fatalf("reachability=%+v", r)
		}
		if l := payload.Lifecycle; l != nil && (!l.Content || l.Direct || l.PreviousGeneration != "99") {
			t.Fatalf("lifecycle=%+v", l)
		}
	}
}

func TestNativeProcessAdmissionExactAllowancePayload(t *testing.T) {
	event, err := clievent.NewNativeConnectivityObserved(clievent.NativeConnectivitySpec{Command: clievent.CommandGet, Side: "receiver", Kind: "admission_granted", State: "unknown", Admission: &clievent.NativeAdmissionFacts{Wait: 12500 * time.Microsecond, Active: 2, Queued: 0, StartsRemaining: 1.25, STUNRemaining: 0.5, ActiveTimeRemaining: 85 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeV3(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(0, 0)}, event)
	if err != nil {
		t.Fatal(err)
	}
	payload := record.Payload.(nativeConnectivityPayloadV3)
	raw, err := json.Marshal(payload.Admission)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"wait_ms":"12.500","active":"2","queued":"0","starts_remaining":"1.25","stun_remaining":"0.5","active_time_remaining_ms":"85000.000"}`
	if string(raw) != want {
		t.Fatalf("admission=%s", raw)
	}
	facts := event.Facts()
	facts.Admission.Queued = 9
	if event.Facts().Admission.Queued != 0 {
		t.Fatal("mutable admission facts escaped")
	}
	facts.Admission.Wait = -1
	if _, err := clievent.NewNativeConnectivityObserved(facts); err == nil {
		t.Fatal("negative allowance duration accepted")
	}
}
