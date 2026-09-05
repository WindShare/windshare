package runtrace

import (
	"net/netip"
	"strconv"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

type nativeConnectivityPayloadV3 struct {
	Kind              string                       `json:"kind"`
	State             string                       `json:"state"`
	Side              string                       `json:"side"`
	AttemptSequence   string                       `json:"attempt_sequence"`
	NetworkGeneration string                       `json:"network_generation_id"`
	Profile           string                       `json:"ice_profile_id"`
	ObservedAt        string                       `json:"observed_at"`
	Candidate         *nativeCandidatePayloadV3    `json:"candidate,omitempty"`
	Pair              *nativePairPayloadV3         `json:"selected_pair,omitempty"`
	Reachability      *nativeReachabilityPayloadV3 `json:"reachability,omitempty"`
	Lifecycle         *nativeLifecyclePayloadV3    `json:"lifecycle,omitempty"`
	Admission         *nativeAdmissionPayloadV3    `json:"admission,omitempty"`
}

func (nativeConnectivityPayloadV3) runTracePayloadV3() {}

type nativeAdmissionPayloadV3 struct {
	Wait                string `json:"wait_ms"`
	Active              string `json:"active"`
	Queued              string `json:"queued"`
	StartsRemaining     string `json:"starts_remaining"`
	STUNRemaining       string `json:"stun_remaining"`
	ActiveTimeRemaining string `json:"active_time_remaining_ms"`
}

type nativeLifecyclePayloadV3 struct {
	Content            bool   `json:"content_demand"`
	Direct             bool   `json:"direct_demand"`
	PreviousGeneration string `json:"previous_network_generation_id"`
}
type nativeCandidatePayloadV3 struct {
	Type           string `json:"type"`
	Protocol       string `json:"protocol"`
	Address        string `json:"address"`
	Port           uint16 `json:"port"`
	Family         string `json:"family"`
	Origin         string `json:"origin"`
	InterfaceClass string `json:"interface_class"`
	STUNEndpoint   string `json:"stun_endpoint"`
	STUNRTT        string `json:"stun_rtt_ms"`
	PolicyDecision string `json:"policy_decision"`
}
type nativePairPayloadV3 struct {
	LocalType     string `json:"local_type"`
	RemoteType    string `json:"remote_type"`
	Protocol      string `json:"protocol"`
	LocalAddress  string `json:"local_address"`
	RemoteAddress string `json:"remote_address"`
	LocalPort     uint16 `json:"local_port"`
	RemotePort    uint16 `json:"remote_port"`
	LocalFamily   string `json:"local_family"`
	RemoteFamily  string `json:"remote_family"`
	PairRTT       string `json:"pair_rtt_ms"`
	Lifetime      string `json:"lifetime_ms"`
	SwitchReason  string `json:"switch_reason"`
}
type nativeReachabilityPayloadV3 struct {
	Local           string `json:"local_endpoint"`
	Remote          string `json:"remote_scope"`
	Protocol        string `json:"protocol"`
	Reason          string `json:"reason"`
	ServerEpoch     uint32 `json:"server_epoch"`
	ServerRestarted bool   `json:"server_restarted"`
}

func (visitor *encodeVisitorV3) VisitNativeConnectivityObserved(event clievent.NativeConnectivityObserved) error {
	facts := event.Facts()
	correlation, err := ProjectCorrelationV1(CorrelationInput{ProtocolSessionID: facts.Session, PeerPathID: facts.Path, PeerAttemptID: facts.Attempt})
	if err != nil {
		return err
	}
	payload := nativeConnectivityPayloadV3{Kind: facts.Kind, State: facts.State, Side: facts.Side, AttemptSequence: nativeKnownUint(facts.AttemptSequence), NetworkGeneration: nativeKnownUint(facts.NetworkGeneration), Profile: facts.Profile, ObservedAt: "unknown"}
	if payload.Profile == "" {
		payload.Profile = "unknown"
	}
	if !facts.At.IsZero() {
		payload.ObservedAt = facts.At.UTC().Format(time.RFC3339Nano)
	}
	if c := facts.Candidate; c != nil {
		payload.Candidate = &nativeCandidatePayloadV3{Type: c.Type, Protocol: c.Protocol, Address: c.Address, Port: c.Port, Family: c.Family, Origin: c.Origin, InterfaceClass: "unknown", STUNEndpoint: "unknown", STUNRTT: "unknown", PolicyDecision: "unknown"}
	}
	if p := facts.Pair; p != nil {
		rtt := "unknown"
		if p.PairRTT > 0 {
			rtt = strconv.FormatFloat(float64(p.PairRTT)/float64(time.Millisecond), 'f', 3, 64)
		}
		payload.Pair = &nativePairPayloadV3{LocalType: p.LocalType, RemoteType: p.RemoteType, Protocol: p.Protocol, LocalAddress: p.LocalAddress, RemoteAddress: p.RemoteAddress, LocalPort: p.LocalPort, RemotePort: p.RemotePort, LocalFamily: nativeFamily(p.LocalAddress), RemoteFamily: nativeFamily(p.RemoteAddress), PairRTT: rtt, Lifetime: "unknown", SwitchReason: "unknown"}
	}
	if r := facts.Reachability; r != nil {
		payload.Reachability = &nativeReachabilityPayloadV3{Local: nativeEndpoint(r.Local), Remote: nativeEndpoint(r.Remote), Protocol: r.Protocol, Reason: r.Reason, ServerEpoch: r.ServerEpoch, ServerRestarted: r.ServerRestarted}
	}
	if l := facts.Lifecycle; l != nil {
		payload.Lifecycle = &nativeLifecyclePayloadV3{Content: l.Content, Direct: l.Direct, PreviousGeneration: nativeKnownUint(l.PreviousGeneration)}
	}
	if a := facts.Admission; a != nil {
		payload.Admission = &nativeAdmissionPayloadV3{Wait: strconv.FormatFloat(float64(a.Wait)/float64(time.Millisecond), 'f', 3, 64), Active: decimal(a.Active), Queued: decimal(a.Queued), StartsRemaining: strconv.FormatFloat(a.StartsRemaining, 'f', -1, 64), STUNRemaining: strconv.FormatFloat(a.STUNRemaining, 'f', -1, 64), ActiveTimeRemaining: strconv.FormatFloat(float64(a.ActiveTimeRemaining)/float64(time.Millisecond), 'f', 3, 64)}
	}
	visitor.set("native_connectivity", correlation, payload)
	return nil
}
func nativeKnownUint(value uint64) string {
	if value == 0 {
		return "unknown"
	}
	return decimal(value)
}
func nativeFamily(value string) string {
	address, err := netip.ParseAddr(value)
	if err != nil {
		return "unknown"
	}
	if address.Is4() {
		return "ipv4"
	}
	return "ipv6"
}
func nativeEndpoint(value netip.AddrPort) string {
	if !value.IsValid() {
		return "unknown"
	}
	return value.String()
}
