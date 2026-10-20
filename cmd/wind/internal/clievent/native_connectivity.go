package clievent

import (
	"encoding/hex"
	"math"
	"net/netip"
	"slices"
	"time"

	"github.com/windshare/windshare/connectivity/turnpath"
)

// NativeConnectivitySpec contains only bounded facts. Raw provider errors and
// signaling descriptions cannot enter the command event contract.
type NativeConnectivitySpec struct {
	Command           Command
	Session           ProtocolSessionID
	Path              PeerPathID
	Attempt           PeerAttemptID
	AttemptSequence   uint64
	NetworkGeneration uint64
	Profile           string
	Side              string
	Kind              string
	State             string
	At                time.Time
	Candidate         *NativeCandidateFacts
	Pair              *NativePairFacts
	Reachability      *NativeReachabilityFacts
	Lifecycle         *NativeLifecycleFacts
	Admission         *NativeAdmissionFacts
	Socket            *NativeSocketFacts
	TURN              *NativeTURNFacts
}

type NativeTURNFacts struct {
	Provider, Account, Profile string
	Milestone, Reason          string
	UsefulBytes                uint64
	Resources                  *NativeTURNResources
	Cleanup                    turnpath.CleanupOutcome
	Binding                    *NativeTURNBinding
	Route                      *NativeTURNRoute
}

type NativeTURNBinding struct {
	Status           string
	Code             int
	Channel          uint16
	Peer, Allocation netip.AddrPort
}

type NativeTURNRoute struct {
	Server  string
	Local   netip.Addr
	Outcome string
}

const (
	maximumTURNTraceURLBytes = 2048
	maximumSTUNErrorCode     = 699
	minimumTURNChannel       = 0x4000
	maximumTURNChannel       = 0x7fff
)

type NativeTURNResources struct {
	ActiveSockets, OpenedSockets     uint64
	TransportSent, TransportReceived uint64
}

func (facts *NativeTURNFacts) clone() *NativeTURNFacts {
	if facts == nil {
		return nil
	}
	copy := *facts
	if copy.Resources != nil {
		resources := *copy.Resources
		copy.Resources = &resources
	}
	if copy.Binding != nil {
		binding := *copy.Binding
		copy.Binding = &binding
	}
	if copy.Route != nil {
		route := *copy.Route
		copy.Route = &route
	}
	return &copy
}

type NativeSocketFacts struct {
	Local, Server netip.AddrPort
	Duration      time.Duration
	Result        string
}

type NativeAdmissionFacts struct {
	Wait                           time.Duration
	Active, Queued                 uint64
	StartsRemaining, STUNRemaining float64
	ActiveTimeRemaining            time.Duration
	SocketCapacity                 NativeSocketCapacityFacts
}

type NativeSocketCapacityFacts struct {
	Used, Reserved, Requested, Limit uint64
}

type NativeLifecycleFacts struct {
	Content, Direct    bool
	PreviousGeneration uint64
}

type NativeCandidateFacts struct {
	Priority                                uint32
	TCPType                                 string
	Type, Protocol, Address, Family, Origin string
	Port                                    uint16
}
type NativePairFacts struct {
	LocalType, RemoteType, Protocol, LocalAddress, RemoteAddress string
	LocalPort, RemotePort                                        uint16
	PairRTT                                                      time.Duration
}
type NativeReachabilityFacts struct {
	Local, Remote   netip.AddrPort
	Protocol        string
	Reason          string
	ServerEpoch     uint32
	ServerRestarted bool
}

type NativeConnectivityObserved struct{ spec NativeConnectivitySpec }

func NewNativeConnectivityObserved(spec NativeConnectivitySpec) (NativeConnectivityObserved, error) {
	if !validNativeConnectivity(spec) {
		return NativeConnectivityObserved{}, ErrInvalidEvent
	}
	if spec.Candidate != nil {
		copied := *spec.Candidate
		spec.Candidate = &copied
	}
	if spec.Pair != nil {
		copied := *spec.Pair
		spec.Pair = &copied
	}
	if spec.Reachability != nil {
		copied := *spec.Reachability
		spec.Reachability = &copied
	}
	if spec.Lifecycle != nil {
		copied := *spec.Lifecycle
		spec.Lifecycle = &copied
	}
	if spec.Admission != nil {
		copied := *spec.Admission
		spec.Admission = &copied
	}
	if spec.Socket != nil {
		copied := *spec.Socket
		spec.Socket = &copied
	}
	spec.TURN = spec.TURN.clone()
	return NativeConnectivityObserved{spec: spec}, nil
}
func (NativeConnectivityObserved) event()                 {}
func (value NativeConnectivityObserved) Command() Command { return value.spec.Command }
func (NativeConnectivityObserved) Level() Level           { return LevelDebug }
func (value NativeConnectivityObserved) Facts() NativeConnectivitySpec {
	spec := value.spec
	if spec.Candidate != nil {
		copied := *spec.Candidate
		spec.Candidate = &copied
	}
	if spec.Pair != nil {
		copied := *spec.Pair
		spec.Pair = &copied
	}
	if spec.Reachability != nil {
		copied := *spec.Reachability
		spec.Reachability = &copied
	}
	if spec.Lifecycle != nil {
		copied := *spec.Lifecycle
		spec.Lifecycle = &copied
	}
	if spec.Admission != nil {
		copied := *spec.Admission
		spec.Admission = &copied
	}
	if spec.Socket != nil {
		copied := *spec.Socket
		spec.Socket = &copied
	}
	spec.TURN = spec.TURN.clone()
	return spec
}
func (value NativeConnectivityObserved) Accept(visitor Visitor) error {
	if visitor == nil || !validNativeConnectivity(value.spec) {
		return ErrInvalidEvent
	}
	return visitor.VisitNativeConnectivityObserved(value)
}
func validNativeConnectivity(spec NativeConnectivitySpec) bool {
	if !spec.Command.Valid() || !slices.Contains([]string{"sender", "receiver", "unknown"}, spec.Side) ||
		!validNativeConnectivityKind(spec) ||
		!slices.Contains([]string{"unknown", "new", "checking", "connected", "completed", "disconnected", "failed", "closed", "connecting"}, spec.State) {
		return false
	}
	if !validNativeSubject(spec) {
		return false
	}
	if !validNativeTURN(spec) {
		return false
	}
	if (spec.Kind == "candidate") != (spec.Candidate != nil) || (spec.Kind == "selected_pair") != (spec.Pair != nil) {
		return false
	}
	isReachability := slices.Contains([]string{"gateway-unavailable", "lease-superseded", "lease-lost", "lease-failed", "lease-ready", "lease-revoked"}, spec.Kind)
	isLifecycle := slices.Contains([]string{"demand_changed", "network_changed", "path_closed"}, spec.Kind)
	if isReachability != (spec.Reachability != nil) || isLifecycle != (spec.Lifecycle != nil) {
		return false
	}
	isAdmission := slices.Contains([]string{"admission_queued", "admission_granted", "admission_released", "admission_rejected"}, spec.Kind)
	if isAdmission != (spec.Admission != nil) {
		return false
	}
	isSocket := slices.Contains([]string{"stun_refresh_finished", "socket_handoff_started", "socket_handoff_finished"}, spec.Kind)
	if isSocket != (spec.Socket != nil) {
		return false
	}
	if s := spec.Socket; s != nil {
		if s.Duration < 0 || !slices.Contains([]string{"pending", "completed", "canceled", "deadline", "retired", "closed", "failed"}, s.Result) ||
			(spec.Kind == "socket_handoff_started") != (s.Result == "pending") {
			return false
		}
	}
	if a := spec.Admission; a != nil && (a.Wait < 0 || a.ActiveTimeRemaining < 0 || !nativeAllowance(a.StartsRemaining) || !nativeAllowance(a.STUNRemaining)) {
		return false
	}
	if c := spec.Candidate; c != nil {
		if !nativeType(c.Type) || !nativeProtocol(c.Protocol) || !nativeAddress(c.Address) ||
			!slices.Contains([]string{"", "active", "passive", "so"}, c.TCPType) ||
			!slices.Contains([]string{"unknown", "ipv4", "ipv6"}, c.Family) || !slices.Contains([]string{"unknown", "ordinary", "mapped"}, c.Origin) {
			return false
		}
	}
	if p := spec.Pair; p != nil {
		if !nativeType(p.LocalType) || !nativeType(p.RemoteType) || !nativeProtocol(p.Protocol) ||
			!nativeAddress(p.LocalAddress) || !nativeAddress(p.RemoteAddress) || p.PairRTT < 0 {
			return false
		}
	}
	if r := spec.Reachability; r != nil {
		if !nativeProtocol(r.Protocol) || !slices.Contains([]string{"none", "unknown", "unavailable", "capacity", "invalid_response", "closed", "lease_lost", "canceled", "deadline"}, r.Reason) {
			return false
		}
	}
	return true
}

func validNativeConnectivityKind(spec NativeConnectivitySpec) bool {
	if spec.TURN != nil && spec.TURN.Cleanup.Valid() && spec.Kind == spec.TURN.Cleanup.EventKind() {
		return true
	}
	return slices.Contains([]string{"turn_path", "turn_binding", "turn_route", "provider_created", "provider_closed", "tcp_unavailable", "gathering_complete", "candidate", "selected_pair", "ice", "peerconnection", "gateway-unavailable", "lease-superseded", "lease-lost", "lease-failed", "lease-ready", "lease-revoked", "demand_changed", "network_changed", "path_closed", "admission_queued", "admission_granted", "admission_released", "admission_rejected", "stun_refresh_finished", "socket_handoff_started", "socket_handoff_finished"}, spec.Kind)
}

func validNativeTURN(spec NativeConnectivitySpec) bool {
	facts := spec.TURN
	if facts == nil {
		return spec.Kind != "turn_path" && spec.Kind != "turn_binding" && spec.Kind != "turn_route"
	}
	if !validTURNRecoveryFacts(spec.Kind, facts) {
		return false
	}
	if facts.Cleanup != 0 && (!facts.Cleanup.Valid() || spec.Kind != facts.Cleanup.EventKind() ||
		facts.Reason != facts.Cleanup.Reason() || facts.Resources == nil) {
		return false
	}
	if len(facts.Reason) > 1024 || !slices.Contains([]string{"", "issuance_started", "issuance_failed", "issuance_discarded", "credentials_delegated", "replacement_admitted", "path_retired", "useful_content", "connecting", "ready", "retired", "unavailable", "recovery_requested"}, facts.Milestone) ||
		(spec.Kind == "turn_path") != (facts.Milestone != "") {
		return false
	}
	for _, id := range []string{facts.Provider, facts.Account, facts.Profile} {
		if len(id) > 64 || (id == "" && facts.Milestone != "unavailable") {
			return false
		}
		for _, char := range id {
			if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' {
				continue
			}
			return false
		}
	}
	return true
}

func validTURNRecoveryFacts(kind string, facts *NativeTURNFacts) bool {
	if (kind == "turn_binding") != (facts.Binding != nil) || (kind == "turn_route") != (facts.Route != nil) {
		return false
	}
	if binding := facts.Binding; binding != nil {
		return slices.Contains([]string{"active", "retrying", "rejected", "mismatch", "expired", "exhausted"}, binding.Status) &&
			binding.Code >= 0 && binding.Code <= maximumSTUNErrorCode && binding.Channel >= minimumTURNChannel && binding.Channel <= maximumTURNChannel &&
			binding.Peer.IsValid() && binding.Allocation.IsValid()
	}
	if route := facts.Route; route != nil {
		return len(route.Server) > 0 && len(route.Server) <= maximumTURNTraceURLBytes &&
			slices.Contains([]string{"started", "allocated", "failed", "canceled", "capacity"}, route.Outcome)
	}
	return true
}
func nativeAllowance(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func nativeType(value string) bool {
	return slices.Contains([]string{"unknown", "host", "srflx", "prflx", "relay"}, value)
}
func nativeProtocol(value string) bool {
	return slices.Contains([]string{"unknown", "udp", "tcp"}, value)
}
func nativeAddress(value string) bool {
	if value == "unknown" {
		return true
	}
	_, err := netip.ParseAddr(value)
	return err == nil
}

func validNativeSubject(spec NativeConnectivitySpec) bool {
	if spec.Attempt.Valid() && (!spec.Path.Valid() || !spec.Session.Valid() || spec.AttemptSequence == 0) {
		return false
	}
	if spec.Path.Valid() && !spec.Session.Valid() {
		return false
	}
	if spec.Profile != "" {
		// Profiles are opaque digests, never URLs that could contain TURN credentials.
		if len(spec.Profile) != 12 || spec.Profile[:4] != "ice-" {
			return false
		}
		if _, err := hex.DecodeString(spec.Profile[4:]); err != nil {
			return false
		}
	}
	return true
}
