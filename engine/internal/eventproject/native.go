package eventproject

import (
	"context"
	"encoding/hex"
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/reachability"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

func ProjectNativeObservation(workflow appobs.Workflow, value nativepeer.Observation) (appobs.NativeConnectivityObserved, error) {
	event, err := projectNativeObservation(workflow, value)
	if err == nil {
		return event, nil
	}
	return event, withRejectionContext(err, nativeObservationRejection(workflow, value))
}

func projectNativeObservation(workflow appobs.Workflow, value nativepeer.Observation) (appobs.NativeConnectivityObserved, error) {
	subject := value.Subject
	spec := appobs.NativeConnectivitySpec{Workflow: workflow, AttemptSequence: subject.AttemptSequence, NetworkGeneration: subject.NetworkGenerationID, Profile: subject.ICEProfileID, Side: subject.Side, State: "unknown"}
	spec.Session, _ = appobs.NewProtocolSessionID(subject.ProtocolSessionID[:])
	spec.Path, _ = appobs.NewPeerPathID(subject.PeerPathID[:])
	spec.Attempt, _ = appobs.NewPeerAttemptID(subject.AttemptID[:])
	if spec.Side == "" {
		spec.Side = "unknown"
	}
	count := 0
	if value.Provider != nil {
		count++
	}
	if value.Reachability != nil {
		count++
	}
	if value.Lifecycle != nil {
		count++
	}
	if value.Admission != nil {
		count++
	}
	if value.Socket != nil {
		count++
	}
	if value.TURN != nil {
		count++
	}
	if count != 1 {
		return appobs.NativeConnectivityObserved{}, appobs.EventContractError{Field: "observation", Rule: "exactly_one_fact"}
	}
	if event := value.TURN; event != nil {
		spec.Kind, spec.At = "turn_path", event.At
		spec.TURN = &appobs.NativeTURNFacts{Provider: event.Provider, Account: event.Account, Profile: event.Profile,
			Milestone: event.Milestone, Reason: event.Reason, UsefulBytes: event.UsefulBytes}
	}
	if socket := value.Socket; socket != nil {
		spec.Kind = string(socket.Kind)
		spec.At = socket.At
		spec.Socket = &appobs.NativeSocketFacts{Local: socket.Local, Server: socket.Server, Duration: socket.Duration, Result: socket.Result}
	}
	if admission := value.Admission; admission != nil {
		if admission.Active < 0 || admission.Queued < 0 {
			return appobs.NativeConnectivityObserved{}, appobs.EventContractError{Field: "admission", Rule: "nonnegative_counts"}
		}
		spec.Kind = "admission_" + string(admission.Kind)
		spec.At = admission.At
		spec.Admission = &appobs.NativeAdmissionFacts{Wait: admission.Wait, Active: uint64(admission.Active), Queued: uint64(admission.Queued), StartsRemaining: admission.StartsRemaining, STUNRemaining: admission.STUNRemaining, ActiveTimeRemaining: admission.ActiveTimeRemaining}
		if capacity := admission.SocketCapacity; capacity != nil {
			spec.Admission.SocketCapacity = appobs.NativeSocketCapacityFacts{Used: uint64(capacity.Used), Reserved: uint64(capacity.Reserved), Requested: uint64(capacity.Requested), Limit: uint64(capacity.Limit)}
		}
	}
	if lifecycle := value.Lifecycle; lifecycle != nil {
		spec.Kind = string(lifecycle.Kind)
		spec.At = lifecycle.At
		spec.Lifecycle = &appobs.NativeLifecycleFacts{Content: lifecycle.Content, Direct: lifecycle.Direct, PreviousGeneration: lifecycle.PreviousGeneration}
	}
	if p := value.Provider; p != nil {
		spec = projectNativeProvider(spec, p)
	}
	if r := value.Reachability; r != nil {
		spec.Kind = r.Kind
		protocol := "unknown"
		switch r.Endpoint.Protocol {
		case reachability.UDP:
			protocol = "udp"
		case reachability.TCP:
			protocol = "tcp"
		}
		spec.Reachability = &appobs.NativeReachabilityFacts{Local: r.Endpoint.Local, Remote: r.Scope.Remote, Protocol: protocol, Reason: nativeReachabilityReason(r.Error), ServerEpoch: r.ServerEpoch, ServerRestarted: r.ServerRestarted}
	}
	return appobs.NewNativeConnectivityObserved(spec)
}

func projectNativeProvider(spec appobs.NativeConnectivitySpec, p *provider.Event) appobs.NativeConnectivitySpec {
	spec.Kind = p.Milestone
	spec.At = p.At
	if d := p.Diagnostic; d != nil {
		spec.Diagnostic = &appobs.NativeProviderDiagnosticFacts{
			Scope: d.Scope, Severity: d.Severity, Message: nativeDiagnosticMessage(d.Message),
		}
	}
	// Setup errors do not describe connection transitions. Diagnostic records
	// carry log text independently of this finite transport-state vocabulary.
	if slices.Contains([]string{"new", "checking", "connected", "completed", "disconnected", "failed", "closed", "connecting"}, p.State) {
		spec.State = p.State
	}
	if c := p.Candidate; c != nil {
		spec.Candidate = &appobs.NativeCandidateFacts{Priority: c.Priority, TCPType: c.TCPType, Type: c.Type, Protocol: c.Protocol, Address: nativeObservedAddress(c.Address), Port: c.Port, Family: c.Family, Origin: c.Origin}
	}
	if pair := p.Pair; pair != nil {
		spec.Pair = &appobs.NativePairFacts{LocalType: pair.LocalType, RemoteType: pair.RemoteType, Protocol: pair.Protocol, LocalAddress: nativeObservedAddress(pair.LocalAddress), RemoteAddress: nativeObservedAddress(pair.RemoteAddress), LocalPort: pair.LocalPort, RemotePort: pair.RemotePort, PairRTT: pair.RoundTripTime}
	}
	if p.TURNService != nil && p.TURN != nil && p.TURN.ActiveSockets >= 0 {
		spec.TURN = &appobs.NativeTURNFacts{Provider: p.TURNService.Provider, Account: p.TURNService.Account, Profile: p.TURNService.Profile,
			Cleanup: p.TURNCleanup, Reason: p.TURNCleanup.Reason(),
			Resources: &appobs.NativeTURNResources{ActiveSockets: uint64(p.TURN.ActiveSockets), OpenedSockets: p.TURN.OpenedSockets,
				TransportSent: p.TURN.TransportSent, TransportReceived: p.TURN.TransportReceived}}
		projectTURNRecoveryFacts(spec.TURN, p)
	}
	return spec
}

func nativeDiagnosticMessage(message string) string {
	message = strings.ToValidUTF8(message, "\uFFFD")
	if len(message) <= appobs.NativeProviderDiagnosticMessageLimit {
		return strings.Clone(message)
	}
	const suffix = "…"
	end := appobs.NativeProviderDiagnosticMessageLimit - len(suffix)
	for !utf8.RuneStart(message[end]) {
		end--
	}
	// Keep a visible truncation marker without retaining the source log buffer.
	return message[:end] + suffix
}

func projectTURNRecoveryFacts(facts *appobs.NativeTURNFacts, event *provider.Event) {
	if binding := event.TURNBinding; binding != nil {
		peer, _ := netip.ParseAddrPort(binding.PeerAddress)
		allocation, _ := netip.ParseAddrPort(binding.RelayedAddress)
		facts.Binding = &appobs.NativeTURNBinding{Status: binding.Status.String(), Code: binding.Code,
			Channel: binding.Channel, Peer: peer, Allocation: allocation}
	}
	if route := event.TURNRoute; route != nil {
		facts.Route = &appobs.NativeTURNRoute{Server: route.Server, Local: route.LocalAddress, Outcome: route.Outcome}
	}
}
func nativeObservedAddress(value string) string {
	if address, err := netip.ParseAddr(value); err == nil {
		return address.String()
	}
	return "unknown"
}
func nativeReachabilityReason(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, reachability.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, reachability.ErrCapacity):
		return "capacity"
	case errors.Is(err, reachability.ErrInvalidResponse):
		return "invalid_response"
	case errors.Is(err, reachability.ErrClosed):
		return "closed"
	case errors.Is(err, reachability.ErrLeaseLost):
		return "lease_lost"
	default:
		return "unknown"
	}
}

func nativeObservationRejection(workflow appobs.Workflow, value nativepeer.Observation) appobs.ObservationRejection {
	subject := value.Subject
	context := appobs.ObservationRejection{
		Event: "native_connectivity", Source: "eventproject.ProjectNativeObservation", Stage: "observation",
		Session:         hex.EncodeToString(subject.ProtocolSessionID[:]),
		AttemptSequence: strconv.FormatUint(subject.AttemptSequence, 10),
	}
	fields := []appobs.RejectionField{
		appobs.RejectedEnum("workflow", uint64(workflow)),
		appobs.RejectedIdentity("protocol_session_id", subject.ProtocolSessionID[:]),
		appobs.RejectedIdentity("peer_path_id", subject.PeerPathID[:]),
		appobs.RejectedIdentity("peer_attempt_id", subject.AttemptID[:]),
		appobs.RejectedUint("attempt_sequence", subject.AttemptSequence),
		appobs.RejectedString("side", subject.Side),
		appobs.RejectedString("ice_profile", subject.ICEProfileID),
	}
	if event := value.TURN; event != nil {
		context.Stage = "turn_path"
		if name, known := event.Milestone.Name(); known {
			context.Stage = name
		}
		fields = append(fields,
			appobs.RejectedEnum("turn.milestone", uint64(event.Milestone)),
			appobs.RejectedString("turn.provider", event.Provider),
			appobs.RejectedString("turn.account", event.Account),
			appobs.RejectedString("turn.profile", event.Profile),
			appobs.RejectedString("turn.reason", event.Reason),
			appobs.RejectedUint("turn.useful_bytes", event.UsefulBytes))
	}
	if event := value.Provider; event != nil {
		context.Stage = event.Milestone
		fields = append(fields, appobs.RejectedString("kind", event.Milestone), appobs.RejectedString("state", event.State))
		if diagnostic := event.Diagnostic; diagnostic != nil {
			fields = append(fields, appobs.RejectedString("diagnostic.scope", diagnostic.Scope),
				appobs.RejectedString("diagnostic.severity", diagnostic.Severity),
				appobs.RejectedString("diagnostic.message", diagnostic.Message))
		}
	}
	if event := value.Socket; event != nil {
		context.Stage = string(event.Kind)
		fields = append(fields, appobs.RejectedString("socket.result", event.Result),
			appobs.RejectedString("socket.duration", event.Duration.String()))
	}
	if event := value.Admission; event != nil {
		context.Stage = "admission_" + string(event.Kind)
		fields = append(fields, appobs.RejectedString("admission.active", strconv.Itoa(event.Active)),
			appobs.RejectedString("admission.queued", strconv.Itoa(event.Queued)))
	}
	if event := value.Lifecycle; event != nil {
		context.Stage = string(event.Kind)
		fields = append(fields, appobs.RejectedString("kind", string(event.Kind)))
	}
	if event := value.Reachability; event != nil {
		context.Stage = event.Kind
		fields = append(fields, appobs.RejectedString("kind", event.Kind))
	}
	return appobs.CaptureObservationRejection(context, fields...)
}
