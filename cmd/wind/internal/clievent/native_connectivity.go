package clievent

import (
	"encoding/hex"
	"math"
	"net/netip"
	"slices"
	"time"
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
}

type NativeAdmissionFacts struct {
	Wait                           time.Duration
	Active, Queued                 uint64
	StartsRemaining, STUNRemaining float64
	ActiveTimeRemaining            time.Duration
}

type NativeLifecycleFacts struct {
	Content, Direct    bool
	PreviousGeneration uint64
}

type NativeCandidateFacts struct {
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
		!slices.Contains([]string{"provider_created", "provider_closed", "tcp_unavailable", "gathering_complete", "candidate", "selected_pair", "ice", "peerconnection", "gateway-unavailable", "lease-superseded", "lease-lost", "lease-failed", "lease-ready", "lease-revoked", "demand_changed", "network_changed", "path_closed", "admission_queued", "admission_granted", "admission_released", "admission_rejected"}, spec.Kind) ||
		!slices.Contains([]string{"unknown", "new", "checking", "connected", "completed", "disconnected", "failed", "closed", "connecting"}, spec.State) {
		return false
	}
	if !validNativeSubject(spec) {
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
	if a := spec.Admission; a != nil && (a.Wait < 0 || a.ActiveTimeRemaining < 0 || !nativeAllowance(a.StartsRemaining) || !nativeAllowance(a.STUNRemaining)) {
		return false
	}
	if c := spec.Candidate; c != nil {
		if !nativeType(c.Type) || !nativeProtocol(c.Protocol) || !nativeAddress(c.Address) ||
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
