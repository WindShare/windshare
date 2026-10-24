package wire

import (
	"net/netip"
	"strconv"
	"time"

	appobs "github.com/windshare/windshare/engine/observation"
)

type NativeConnectivityPayload struct {
	Kind              string                     `json:"kind"`
	State             string                     `json:"state"`
	Side              string                     `json:"side"`
	AttemptSequence   string                     `json:"attempt_sequence"`
	NetworkGeneration string                     `json:"network_generation_id"`
	Profile           string                     `json:"ice_profile_id"`
	ObservedAt        string                     `json:"observed_at"`
	Diagnostic        *NativeProviderDiagnostic  `json:"diagnostic,omitempty"`
	Candidate         *NativeCandidatePayload    `json:"candidate,omitempty"`
	Pair              *NativePairPayload         `json:"selected_pair,omitempty"`
	Reachability      *NativeReachabilityPayload `json:"reachability,omitempty"`
	Lifecycle         *NativeLifecyclePayload    `json:"lifecycle,omitempty"`
	Admission         *NativeAdmissionPayload    `json:"admission,omitempty"`
	Socket            *NativeSocketPayload       `json:"socket,omitempty"`
	TURN              *NativeTURNPayload         `json:"turn,omitempty"`
	Parallelism       *NativeParallelismPayload  `json:"parallelism,omitempty"`
}

type NativeParallelismPayload struct {
	TrialPath            string  `json:"trial_path,omitempty"`
	TrialLaneID          uint32  `json:"trial_lane_id,omitempty"`
	TrialLaneEpoch       uint32  `json:"trial_lane_epoch,omitempty"`
	OfferedBytes         string  `json:"offered_bytes"`
	SettledBytes         string  `json:"settled_bytes"`
	CompletedBytes       string  `json:"completed_bytes"`
	CompletedAttempts    string  `json:"completed_attempts"`
	FailedAttempts       string  `json:"failed_attempts"`
	SupersededAttempts   string  `json:"superseded_attempts"`
	CancelledAttempts    string  `json:"cancelled_attempts"`
	BusyMilliseconds     string  `json:"busy_ms"`
	NetworkWaitingBytes  string  `json:"network_waiting_bytes"`
	InFlightBytes        string  `json:"in_flight_bytes"`
	RemainingBytes       string  `json:"remaining_bytes"`
	RemainingKnown       bool    `json:"remaining_known"`
	Action               string  `json:"action"`
	Reason               string  `json:"reason"`
	Paths                string  `json:"paths"`
	OptionalPaths        string  `json:"optional_paths"`
	QueuedBytes          string  `json:"queued_bytes"`
	UnfinishedBytes      string  `json:"unfinished_bytes"`
	CapacityWaitingBytes string  `json:"capacity_waiting_bytes"`
	UsefulRate           float64 `json:"useful_rate"`
	OutputRate           float64 `json:"output_rate"`
	BaselineUsefulRate   float64 `json:"baseline_useful_rate"`
	BaselineOutputRate   float64 `json:"baseline_output_rate"`
}

type NativeProviderDiagnostic struct {
	Scope    string `json:"scope"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

func (NativeConnectivityPayload) ObservationPayload() {}

type NativeTURNPayload struct {
	Provider    string                      `json:"provider"`
	Account     string                      `json:"account"`
	Profile     string                      `json:"profile"`
	Milestone   string                      `json:"milestone,omitempty"`
	Reason      string                      `json:"reason,omitempty"`
	UsefulBytes string                      `json:"useful_bytes,omitempty"`
	Resources   *NativeTURNResourcesPayload `json:"resources,omitempty"`
	Binding     *NativeTURNBindingPayload   `json:"binding,omitempty"`
	Route       *NativeTURNRoutePayload     `json:"route,omitempty"`
}
type NativeTURNBindingPayload struct {
	Status     string `json:"status"`
	Code       uint16 `json:"code"`
	Channel    uint16 `json:"channel"`
	Peer       string `json:"peer"`
	Allocation string `json:"allocation"`
}
type NativeTURNRoutePayload struct {
	Server  string `json:"server"`
	Local   string `json:"local"`
	Outcome string `json:"outcome"`
}
type NativeTURNResourcesPayload struct {
	ActiveSockets     string `json:"active_sockets"`
	OpenedSockets     string `json:"opened_sockets"`
	TransportSent     string `json:"transport_sent_bytes"`
	TransportReceived string `json:"transport_received_bytes"`
}
type NativeSocketPayload struct {
	Local    string `json:"local_endpoint"`
	Server   string `json:"stun_server"`
	Duration string `json:"duration_ms"`
	Result   string `json:"result"`
}

type NativeAdmissionPayload struct {
	Purpose             string                       `json:"purpose,omitempty"`
	Wait                string                       `json:"wait_ms"`
	Active              string                       `json:"active"`
	Queued              string                       `json:"queued"`
	StartsRemaining     string                       `json:"starts_remaining"`
	STUNRemaining       string                       `json:"stun_remaining"`
	ActiveTimeRemaining string                       `json:"active_time_remaining_ms"`
	SocketCapacity      *NativeSocketCapacityPayload `json:"socket_capacity,omitempty"`
}

type NativeSocketCapacityPayload struct {
	Used      string `json:"used"`
	Reserved  string `json:"reserved"`
	Requested string `json:"requested"`
	Limit     string `json:"limit"`
}

type NativeLifecyclePayload struct {
	Content            bool   `json:"content_demand"`
	Direct             bool   `json:"direct_demand"`
	PreviousGeneration string `json:"previous_network_generation_id"`
}
type NativeCandidatePayload struct {
	Priority       uint32 `json:"priority"`
	TCPType        string `json:"tcp_type,omitempty"`
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
type NativePairPayload struct {
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
type NativeReachabilityPayload struct {
	Local           string `json:"local_endpoint"`
	Remote          string `json:"remote_scope"`
	Protocol        string `json:"protocol"`
	Reason          string `json:"reason"`
	ServerEpoch     uint32 `json:"server_epoch"`
	ServerRestarted bool   `json:"server_restarted"`
}

func (enc *encoder) encodeNativeConnectivityObserved(event appobs.NativeConnectivityObserved) error {
	facts := event.Facts()
	correlation, err := ProjectCorrelationV1(CorrelationInput{ProtocolSessionID: facts.Session, PeerPathID: facts.Path, PeerAttemptID: facts.Attempt})
	if err != nil {
		return err
	}
	payload := NativeConnectivityPayload{Kind: facts.Kind, State: facts.State, Side: facts.Side, AttemptSequence: nativeKnownUint(facts.AttemptSequence), NetworkGeneration: nativeKnownUint(facts.NetworkGeneration), Profile: facts.Profile, ObservedAt: "unknown"}
	if payload.Profile == "" {
		payload.Profile = "unknown"
	}
	if !facts.At.IsZero() {
		payload.ObservedAt = facts.At.UTC().Format(time.RFC3339Nano)
	}
	if d := facts.Diagnostic; d != nil {
		payload.Diagnostic = &NativeProviderDiagnostic{Scope: d.Scope, Severity: d.Severity, Message: d.Message}
	}
	if c := facts.Candidate; c != nil {
		payload.Candidate = &NativeCandidatePayload{Priority: c.Priority, TCPType: c.TCPType, Type: c.Type, Protocol: c.Protocol, Address: c.Address, Port: c.Port, Family: c.Family, Origin: c.Origin, InterfaceClass: "unknown", STUNEndpoint: "unknown", STUNRTT: "unknown", PolicyDecision: "unknown"}
	}
	if p := facts.Pair; p != nil {
		rtt := "unknown"
		if p.PairRTT > 0 {
			rtt = strconv.FormatFloat(float64(p.PairRTT)/float64(time.Millisecond), 'f', 3, 64)
		}
		payload.Pair = &NativePairPayload{LocalType: p.LocalType, RemoteType: p.RemoteType, Protocol: p.Protocol, LocalAddress: p.LocalAddress, RemoteAddress: p.RemoteAddress, LocalPort: p.LocalPort, RemotePort: p.RemotePort, LocalFamily: nativeFamily(p.LocalAddress), RemoteFamily: nativeFamily(p.RemoteAddress), PairRTT: rtt, Lifetime: "unknown", SwitchReason: "unknown"}
	}
	if r := facts.Reachability; r != nil {
		payload.Reachability = &NativeReachabilityPayload{Local: nativeEndpoint(r.Local), Remote: nativeEndpoint(r.Remote), Protocol: r.Protocol, Reason: r.Reason, ServerEpoch: r.ServerEpoch, ServerRestarted: r.ServerRestarted}
	}
	if l := facts.Lifecycle; l != nil {
		payload.Lifecycle = &NativeLifecyclePayload{Content: l.Content, Direct: l.Direct, PreviousGeneration: nativeKnownUint(l.PreviousGeneration)}
	}
	if p := facts.Parallelism; p != nil {
		payload.Parallelism = &NativeParallelismPayload{Action: p.Action, Reason: p.Reason, Paths: decimal(p.Paths), OptionalPaths: decimal(p.OptionalPaths),
			NetworkWaitingBytes: decimal(p.NetworkWaitingBytes), InFlightBytes: decimal(p.InFlightBytes), RemainingBytes: decimal(p.RemainingBytes), RemainingKnown: p.RemainingKnown,
			QueuedBytes: decimal(p.QueuedBytes), UnfinishedBytes: decimal(p.UnfinishedBytes), CapacityWaitingBytes: decimal(p.CapacityWaitingBytes),
			UsefulRate: p.UsefulRate, OutputRate: p.OutputRate, BaselineUsefulRate: p.BaselineUsefulRate, BaselineOutputRate: p.BaselineOutputRate,
			TrialPath: p.TrialPath, TrialLaneID: p.TrialLaneID, TrialLaneEpoch: p.TrialLaneEpoch,
			OfferedBytes: decimal(p.OfferedBytes), SettledBytes: decimal(p.SettledBytes), CompletedBytes: decimal(p.CompletedBytes),
			CompletedAttempts: decimal(p.CompletedAttempts), FailedAttempts: decimal(p.FailedAttempts),
			SupersededAttempts: decimal(p.SupersededAttempts), CancelledAttempts: decimal(p.CancelledAttempts), BusyMilliseconds: decimal(p.BusyMilliseconds)}
	}
	if a := facts.Admission; a != nil {
		payload.Admission = &NativeAdmissionPayload{Purpose: a.Purpose, Wait: strconv.FormatFloat(float64(a.Wait)/float64(time.Millisecond), 'f', 3, 64), Active: decimal(a.Active), Queued: decimal(a.Queued), StartsRemaining: strconv.FormatFloat(a.StartsRemaining, 'f', -1, 64), STUNRemaining: strconv.FormatFloat(a.STUNRemaining, 'f', -1, 64), ActiveTimeRemaining: strconv.FormatFloat(float64(a.ActiveTimeRemaining)/float64(time.Millisecond), 'f', 3, 64)}
	}
	if s := facts.Socket; s != nil {
		payload.Socket = &NativeSocketPayload{Local: nativeEndpoint(s.Local), Server: nativeEndpoint(s.Server), Duration: strconv.FormatFloat(float64(s.Duration)/float64(time.Millisecond), 'f', 3, 64), Result: s.Result}
	}
	payload.TURN = encodeNativeTURN(facts.TURN)
	if a := facts.Admission; a != nil && a.SocketCapacity.Limit != 0 {
		c := a.SocketCapacity
		payload.Admission.SocketCapacity = &NativeSocketCapacityPayload{Used: decimal(c.Used), Reserved: decimal(c.Reserved), Requested: decimal(c.Requested), Limit: decimal(c.Limit)}
	}
	enc.set("native_connectivity", correlation, payload)
	return nil
}

func encodeNativeTURN(turn *appobs.NativeTURNFacts) *NativeTURNPayload {
	if turn == nil {
		return nil
	}
	payload := &NativeTURNPayload{Provider: turn.Provider, Account: turn.Account, Profile: turn.Profile,
		Milestone: turn.Milestone.String(), Reason: turn.Reason}
	if turn.Milestone != 0 {
		payload.UsefulBytes = decimal(turn.UsefulBytes)
	}
	if resources := turn.Resources; resources != nil {
		payload.Resources = &NativeTURNResourcesPayload{ActiveSockets: decimal(resources.ActiveSockets), OpenedSockets: decimal(resources.OpenedSockets),
			TransportSent: decimal(resources.TransportSent), TransportReceived: decimal(resources.TransportReceived)}
	}
	if binding := turn.Binding; binding != nil {
		payload.Binding = &NativeTURNBindingPayload{Status: binding.Status, Code: uint16(binding.Code),
			Channel: binding.Channel, Peer: binding.Peer.String(), Allocation: binding.Allocation.String()}
	}
	if route := turn.Route; route != nil {
		local := "system"
		if route.Local.IsValid() {
			local = route.Local.String()
		}
		payload.Route = &NativeTURNRoutePayload{Server: route.Server, Local: local, Outcome: route.Outcome}
	}
	return payload
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
