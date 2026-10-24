package observation

import (
	"encoding/hex"
	"math"
	"net/netip"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/windshare/windshare/core/connectionpath"
)

type NativeConnectivitySpec struct {
	Workflow          Workflow
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
	Diagnostic        *NativeProviderDiagnosticFacts
	Candidate         *NativeCandidateFacts
	Pair              *NativePairFacts
	Reachability      *NativeReachabilityFacts
	Lifecycle         *NativeLifecycleFacts
	Admission         *NativeAdmissionFacts
	Socket            *NativeSocketFacts
	TURN              *NativeTURNFacts
	Parallelism       *NativeParallelismFacts
}

type NativeParallelismFacts struct {
	TrialPath                                                                string
	TrialLaneID, TrialLaneEpoch                                              uint32
	OfferedBytes, SettledBytes, CompletedBytes                               uint64
	CompletedAttempts, FailedAttempts, SupersededAttempts, CancelledAttempts uint64
	BusyMilliseconds                                                         uint64
	NetworkWaitingBytes, InFlightBytes, RemainingBytes                       uint64
	RemainingKnown                                                           bool
	Action, Reason                                                           string
	Paths, OptionalPaths                                                     uint64
	QueuedBytes, UnfinishedBytes, CapacityWaitingBytes                       uint64
	UsefulRate, OutputRate, BaselineUsefulRate, BaselineOutputRate           float64
}

const NativeProviderDiagnosticMessageLimit = 4096

type NativeProviderDiagnosticFacts struct {
	Scope    string
	Severity string
	Message  string
}

type NativeTURNFacts struct {
	Provider, Account, Profile string
	Milestone                  connectionpath.Milestone
	Reason                     string
	UsefulBytes                uint64
	Resources                  *NativeTURNResources
	Cleanup                    connectionpath.CleanupOutcome
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
	maximumTURNReasonBytes   = 1024
	maximumTURNIdentityBytes = 64
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
	Purpose                        string
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
	if err := validateNativeConnectivity(spec); err != nil {
		return NativeConnectivityObserved{}, err
	}
	if spec.Diagnostic != nil {
		copied := *spec.Diagnostic
		spec.Diagnostic = &copied
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
	if spec.Parallelism != nil {
		copied := *spec.Parallelism
		spec.Parallelism = &copied
	}
	spec.TURN = spec.TURN.clone()
	return NativeConnectivityObserved{spec: spec}, nil
}

func (NativeConnectivityObserved) EngineEvent() {}

func (value NativeConnectivityObserved) Workflow() Workflow { return value.spec.Workflow }

func (NativeConnectivityObserved) Level() Level { return LevelDebug }

func (value NativeConnectivityObserved) Facts() NativeConnectivitySpec {
	spec := value.spec
	if spec.Diagnostic != nil {
		copied := *spec.Diagnostic
		spec.Diagnostic = &copied
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
	if spec.Parallelism != nil {
		copied := *spec.Parallelism
		spec.Parallelism = &copied
	}
	spec.TURN = spec.TURN.clone()
	return spec
}

func (value NativeConnectivityObserved) Validate() error {
	if err := validateNativeConnectivity(value.spec); err != nil {
		return err
	}
	return nil
}

func validateNativeConnectivity(spec NativeConnectivitySpec) error {
	if !spec.Workflow.Valid() {
		return EventContractError{Field: "workflow", Rule: "known_enum"}
	}
	if !slices.Contains([]string{"sender", "receiver", "unknown"}, spec.Side) {
		return EventContractError{Field: "side", Rule: "known_enum"}
	}
	if !validNativeConnectivityKind(spec) {
		return EventContractError{Field: "kind", Rule: "known_enum"}
	}
	if !slices.Contains([]string{"unknown", "new", "checking", "connected", "completed", "disconnected", "failed", "closed", "connecting"}, spec.State) {
		return EventContractError{Field: "state", Rule: "known_enum"}
	}
	if err := validateNativeSubject(spec); err != nil {
		return err
	}
	if err := validateNativeTURN(spec); err != nil {
		return err
	}
	if !validNativeProviderDiagnostic(spec) {
		return EventContractError{Field: "diagnostic", Rule: "bounded_provider_diagnostic"}
	}
	if err := validateNativeFactKinds(spec); err != nil {
		return err
	}
	if s := spec.Socket; s != nil {
		if s.Duration < 0 || !slices.Contains([]string{"pending", "completed", "canceled", "deadline", "retired", "closed", "failed"}, s.Result) ||
			(spec.Kind == "socket_handoff_started") != (s.Result == "pending") {
			return EventContractError{Field: "socket", Rule: "valid_result_duration"}
		}
	}
	if a := spec.Admission; a != nil && (a.Wait < 0 || a.ActiveTimeRemaining < 0 || !nativeAllowance(a.StartsRemaining) || !nativeAllowance(a.STUNRemaining)) {
		return EventContractError{Field: "admission", Rule: "nonnegative_finite_allowance"}
	}
	if c := spec.Candidate; c != nil {
		if !nativeType(c.Type) || !nativeProtocol(c.Protocol) || !nativeAddress(c.Address) ||
			!slices.Contains([]string{"", "active", "passive", "so"}, c.TCPType) ||
			!slices.Contains([]string{"unknown", "ipv4", "ipv6"}, c.Family) || !slices.Contains([]string{"unknown", "ordinary", "mapped"}, c.Origin) {
			return EventContractError{Field: "candidate", Rule: "valid_candidate"}
		}
	}
	if p := spec.Pair; p != nil {
		if !nativeType(p.LocalType) || !nativeType(p.RemoteType) || !nativeProtocol(p.Protocol) ||
			!nativeAddress(p.LocalAddress) || !nativeAddress(p.RemoteAddress) || p.PairRTT < 0 {
			return EventContractError{Field: "pair", Rule: "valid_selected_pair"}
		}
	}
	if r := spec.Reachability; r != nil {
		if !nativeProtocol(r.Protocol) || !slices.Contains([]string{"none", "unknown", "unavailable", "capacity", "invalid_response", "closed", "lease_lost", "canceled", "deadline"}, r.Reason) {
			return EventContractError{Field: "reachability", Rule: "known_protocol_reason"}
		}
	}
	return nil
}

func validateNativeFactKinds(spec NativeConnectivitySpec) error {
	if (spec.Kind == "parallelism") != (spec.Parallelism != nil) {
		return EventContractError{Field: "parallelism", Rule: "kind_matches_facts"}
	}
	if p := spec.Parallelism; p != nil {
		if !slices.Contains([]string{"explore", "sample", "keep", "retire", "abandon"}, p.Action) || len(p.Reason) > 64 ||
			!nativeAllowance(p.UsefulRate) || !nativeAllowance(p.OutputRate) || !nativeAllowance(p.BaselineUsefulRate) || !nativeAllowance(p.BaselineOutputRate) {
			return EventContractError{Field: "parallelism", Rule: "valid_decision_rates"}
		}
	}
	if (spec.Kind == "candidate") != (spec.Candidate != nil) || (spec.Kind == "selected_pair") != (spec.Pair != nil) {
		return EventContractError{Field: "candidate_pair", Rule: "kind_matches_facts"}
	}
	isReachability := slices.Contains([]string{"gateway-unavailable", "lease-superseded", "lease-lost", "lease-failed", "lease-ready", "lease-revoked"}, spec.Kind)
	isLifecycle := slices.Contains([]string{"demand_changed", "network_changed", "path_closed"}, spec.Kind)
	if isReachability != (spec.Reachability != nil) || isLifecycle != (spec.Lifecycle != nil) {
		return EventContractError{Field: "reachability_lifecycle", Rule: "kind_matches_facts"}
	}
	isAdmission := slices.Contains([]string{"admission_queued", "admission_granted", "admission_released", "admission_rejected"}, spec.Kind)
	if isAdmission != (spec.Admission != nil) {
		return EventContractError{Field: "admission", Rule: "kind_matches_facts"}
	}
	isSocket := slices.Contains([]string{"stun_refresh_finished", "socket_handoff_started", "socket_handoff_finished"}, spec.Kind)
	if isSocket != (spec.Socket != nil) {
		return EventContractError{Field: "socket", Rule: "kind_matches_facts"}
	}
	return nil
}

func validNativeProviderDiagnostic(spec NativeConnectivitySpec) bool {
	if spec.Diagnostic == nil {
		return spec.Kind != "provider_diagnostic"
	}
	d := spec.Diagnostic
	return spec.Kind == "provider_diagnostic" && spec.State == "unknown" &&
		slices.Contains([]string{"ice", "turnc"}, d.Scope) &&
		slices.Contains([]string{"warning", "error"}, d.Severity) &&
		len(d.Message) <= NativeProviderDiagnosticMessageLimit && utf8.ValidString(d.Message)
}

func validNativeConnectivityKind(spec NativeConnectivitySpec) bool {
	if spec.Kind == "provider_diagnostic" || spec.Kind == "parallelism" {
		return true
	}
	if spec.TURN != nil && spec.TURN.Cleanup.Valid() && spec.Kind == spec.TURN.Cleanup.EventKind() {
		return true
	}
	return slices.Contains([]string{"turn_path", "turn_binding", "turn_route", "provider_created", "provider_closed", "tcp_unavailable", "gathering_complete", "candidate", "selected_pair", "ice", "peerconnection", "gateway-unavailable", "lease-superseded", "lease-lost", "lease-failed", "lease-ready", "lease-revoked", "demand_changed", "network_changed", "path_closed", "admission_queued", "admission_granted", "admission_released", "admission_rejected", "stun_refresh_finished", "socket_handoff_started", "socket_handoff_finished"}, spec.Kind)
}

func validateNativeTURN(spec NativeConnectivitySpec) error {
	facts := spec.TURN
	if facts == nil {
		if spec.Kind == "turn_path" || spec.Kind == "turn_binding" || spec.Kind == "turn_route" {
			return EventContractError{Field: "turn", Rule: "required"}
		}
		return nil
	}
	if !validTURNRecoveryFacts(spec.Kind, facts) {
		return EventContractError{Field: "turn.recovery", Rule: "valid_binding_route"}
	}
	if facts.Cleanup != 0 && (!facts.Cleanup.Valid() || spec.Kind != facts.Cleanup.EventKind() ||
		facts.Reason != facts.Cleanup.Reason() || facts.Resources == nil) {
		return EventContractError{Field: "turn.cleanup", Rule: "outcome_matches_kind_reason_resources"}
	}
	if len(facts.Reason) > maximumTURNReasonBytes {
		return EventContractError{Field: "turn.reason", Rule: "bounded_text"}
	}
	if spec.Kind == "turn_path" {
		if _, known := facts.Milestone.Name(); !known {
			return EventContractError{Field: "turn.milestone", Rule: "known_enum"}
		}
	} else if facts.Milestone != 0 {
		return EventContractError{Field: "turn.milestone", Rule: "only_turn_path"}
	}
	for _, identity := range [...]struct{ field, value string }{
		{"turn.provider", facts.Provider}, {"turn.account", facts.Account}, {"turn.profile", facts.Profile},
	} {
		id := identity.value
		if len(id) > maximumTURNIdentityBytes || (id == "" && facts.Milestone.RequiresServiceIdentity()) {
			return EventContractError{Field: identity.field, Rule: "bounded_service_identity"}
		}
		for _, char := range id {
			if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' {
				continue
			}
			return EventContractError{Field: identity.field, Rule: "ascii_service_identity"}
		}
	}
	return nil
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

func validateNativeSubject(spec NativeConnectivitySpec) error {
	if spec.Attempt.Valid() && (!spec.Path.Valid() || !spec.Session.Valid() || spec.AttemptSequence == 0) {
		return EventContractError{Field: "attempt", Rule: "requires_session_path_sequence"}
	}
	if spec.Path.Valid() && !spec.Session.Valid() {
		return EventContractError{Field: "path", Rule: "requires_session"}
	}
	if spec.Profile != "" {
		// Profiles are opaque digests, never URLs that could contain TURN credentials.
		if len(spec.Profile) != 12 || spec.Profile[:4] != "ice-" {
			return EventContractError{Field: "ice_profile", Rule: "ice_digest"}
		}
		if _, err := hex.DecodeString(spec.Profile[4:]); err != nil {
			return EventContractError{Field: "ice_profile", Rule: "ice_digest"}
		}
	}
	return nil
}
