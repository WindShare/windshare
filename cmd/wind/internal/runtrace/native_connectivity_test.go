package runtrace

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestNativeConnectivityExactPayloadAndCorrelation(t *testing.T) {
	session, _ := appobs.NewProtocolSessionID(append([]byte{1}, make([]byte, 15)...))
	path, _ := appobs.NewPeerPathID(append([]byte{2}, make([]byte, 15)...))
	attempt, _ := appobs.NewPeerAttemptID(append([]byte{3}, make([]byte, 15)...))
	spec := appobs.NativeConnectivitySpec{Workflow: appobs.WorkflowReceive, Session: session, Path: path, Attempt: attempt, AttemptSequence: 9007199254740993, NetworkGeneration: 9007199254740994, Profile: "ice-0123abcd", Side: "receiver", Kind: "candidate", State: "unknown", At: time.Unix(0, 0), Candidate: &appobs.NativeCandidateFacts{Priority: 1224736767, TCPType: "passive", Type: "srflx", Protocol: "tcp", Address: "8.8.8.8", Port: 123, Family: "ipv4", Origin: "ordinary"}}
	event, err := clievent.NewNativeConnectivityObserved(spec)
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeV4(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(0, 0)}, event)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(record.Payload)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"candidate","state":"unknown","side":"receiver","attempt_sequence":"9007199254740993","network_generation_id":"9007199254740994","ice_profile_id":"ice-0123abcd","observed_at":"1970-01-01T00:00:00Z","candidate":{"priority":1224736767,"tcp_type":"passive","type":"srflx","protocol":"tcp","address":"8.8.8.8","port":123,"family":"ipv4","origin":"ordinary","interface_class":"unknown","stun_endpoint":"unknown","stun_rtt_ms":"unknown","policy_decision":"unknown"}}`
	if string(raw) != want {
		t.Fatalf("payload=%s", raw)
	}
	if record.Event != "native_connectivity" || record.Correlation.ProtocolSessionID != "AQAAAAAAAAAAAAAAAAAAAA" || record.Correlation.PeerPathID != "AgAAAAAAAAAAAAAAAAAAAA" || record.Correlation.PeerAttemptID != "AwAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("record=%+v", record)
	}
}

func TestNativeConnectivityUnknownsRemainDistinctFromPairMeasurements(t *testing.T) {
	for _, rtt := range []time.Duration{0, 12500 * time.Microsecond} {
		spec := appobs.NativeConnectivitySpec{Workflow: appobs.WorkflowShare, Side: "sender", Kind: "selected_pair", State: "unknown", Pair: &appobs.NativePairFacts{LocalType: "host", RemoteType: "relay", Protocol: "tcp", LocalAddress: "::1", RemoteAddress: "unknown", PairRTT: rtt}}
		event, err := clievent.NewNativeConnectivityObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		record, err := encodeV4(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(0, 0)}, event)
		if err != nil {
			t.Fatal(err)
		}
		payload := record.Payload.(nativeConnectivityPayloadV4)
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
	for _, spec := range []appobs.NativeConnectivitySpec{
		{Workflow: appobs.WorkflowReceive, Side: "unknown", Kind: "lease-ready", State: "unknown", Reachability: &appobs.NativeReachabilityFacts{Protocol: "udp", Reason: "none", Local: netip.MustParseAddrPort("127.0.0.1:123"), ServerEpoch: 42, ServerRestarted: true}},
		{Workflow: appobs.WorkflowReceive, Side: "receiver", Kind: "network_changed", State: "unknown", Lifecycle: &appobs.NativeLifecycleFacts{Content: true, Direct: false, PreviousGeneration: 99}},
	} {
		event, err := clievent.NewNativeConnectivityObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		record, err := encodeV4(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(0, 0)}, event)
		if err != nil {
			t.Fatal(err)
		}
		payload := record.Payload.(nativeConnectivityPayloadV4)
		if r := payload.Reachability; r != nil && (r.Local != "127.0.0.1:123" || r.Remote != "unknown" || r.ServerEpoch != 42 || !r.ServerRestarted) {
			t.Fatalf("reachability=%+v", r)
		}
		if l := payload.Lifecycle; l != nil && (!l.Content || l.Direct || l.PreviousGeneration != "99") {
			t.Fatalf("lifecycle=%+v", l)
		}
	}
}

func TestNativeProcessAdmissionExactAllowancePayload(t *testing.T) {
	event, err := clievent.NewNativeConnectivityObserved(appobs.NativeConnectivitySpec{Workflow: appobs.WorkflowReceive, Side: "receiver", Kind: "admission_granted", State: "unknown", Admission: &appobs.NativeAdmissionFacts{Wait: 12500 * time.Microsecond, Active: 2, Queued: 0, StartsRemaining: 1.25, STUNRemaining: 0.5, ActiveTimeRemaining: 85 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeV4(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(0, 0)}, event)
	if err != nil {
		t.Fatal(err)
	}
	payload := record.Payload.(nativeConnectivityPayloadV4)
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
	facts.Kind = "admission_queued"
	facts.Admission.SocketCapacity = appobs.NativeSocketCapacityFacts{Used: 58, Requested: 13, Limit: 64}
	blocked, err := clievent.NewNativeConnectivityObserved(facts)
	if err != nil {
		t.Fatal(err)
	}
	record, err = encodeV4(testRunIdentity(1), entryMetadata{sequence: 2, time: time.Unix(0, 0)}, blocked)
	if err != nil {
		t.Fatal(err)
	}
	capacity := record.Payload.(nativeConnectivityPayloadV4).Admission.SocketCapacity
	if capacity == nil || capacity.Used != "58" || capacity.Reserved != "0" || capacity.Requested != "13" || capacity.Limit != "64" {
		t.Fatal("capacity decision lost from trace", capacity)
	}
	facts.Admission.Wait = -1
	if _, err := clievent.NewNativeConnectivityObserved(facts); err == nil {
		t.Fatal("negative allowance duration accepted")
	}
}

func TestParallelismTraceRetainsExactBacklogAndAggregateRates(t *testing.T) {
	event, err := clievent.NewNativeConnectivityObserved(appobs.NativeConnectivitySpec{
		Workflow: appobs.WorkflowReceive, Side: "receiver", Kind: "parallelism", State: "unknown",
		Parallelism: &appobs.NativeParallelismFacts{Action: "retire", Reason: "no_gain", Paths: 2, OptionalPaths: 1,
			QueuedBytes: 9007199254740993, UnfinishedBytes: 100, CapacityWaitingBytes: 50,
			UsefulRate: 12.5, OutputRate: 11.25, BaselineUsefulRate: 12, BaselineOutputRate: 11},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeV4(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(0, 0)}, event)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(record.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	var decision map[string]any
	if err := json.Unmarshal(payload["parallelism"], &decision); err != nil {
		t.Fatal(err)
	}
	if decision["queued_bytes"] != "9007199254740993" || decision["useful_rate"] != 12.5 || decision["output_rate"] != 11.25 || decision["reason"] != "no_gain" {
		t.Fatalf("parallelism trace lost exact backlog or rates: %s", raw)
	}
}

func TestNativeTURNTraceSeparatesSocketCountersFromUsefulBytes(t *testing.T) {
	event, err := clievent.NewNativeConnectivityObserved(appobs.NativeConnectivitySpec{
		Workflow: appobs.WorkflowShare, Side: "sender", Kind: "provider_closed", State: "unknown",
		TURN: &appobs.NativeTURNFacts{Provider: "cloudflare", Account: "owner", Profile: "primary",
			Resources: &appobs.NativeTURNResources{OpenedSockets: 2, TransportSent: 9007199254740993, TransportReceived: 300}},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeV4(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(0, 0)}, event)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(record.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		TURN struct {
			Provider  string `json:"provider"`
			Resources struct {
				Sent   string `json:"transport_sent_bytes"`
				Active string `json:"active_sockets"`
			} `json:"resources"`
		} `json:"turn"`
	}
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.TURN.Provider != "cloudflare" || wire.TURN.Resources.Sent != "9007199254740993" || wire.TURN.Resources.Active != "0" ||
		strings.Contains(string(raw), "useful_bytes") || strings.Contains(string(raw), "billed") {
		t.Fatalf("invented usage or lost integer precision: %s", raw)
	}
}
