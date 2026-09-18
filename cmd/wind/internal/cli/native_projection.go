package cli

import (
	"context"
	"errors"
	"net/netip"
	"slices"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/internal/testrun"
)

func (a *App) observeSenderPeerAttempt(observation v2peer.SenderAttemptObservation) {
	if observation.Stage != v2peer.SenderAttemptAdmitted || observation.Lane == nil {
		return
	}
	// SenderAttemptAdmitted is emitted only after authenticated runtime ownership,
	// making it the sender-side synchronization counterpart to receiver Ready.
	a.recordProcessTrace(
		processTraceShareComponent,
		processTraceSenderDirectLane,
		testrun.OutcomeSucceeded,
	)
}

func projectNativeObservation(command clievent.Command, value nativepeer.Observation) (clievent.NativeConnectivityObserved, error) {
	subject := value.Subject
	spec := clievent.NativeConnectivitySpec{Command: command, AttemptSequence: subject.AttemptSequence, NetworkGeneration: subject.NetworkGenerationID, Profile: subject.ICEProfileID, Side: subject.Side, State: "unknown"}
	spec.Session, _ = clievent.NewProtocolSessionID(subject.ProtocolSessionID[:])
	spec.Path, _ = clievent.NewPeerPathID(subject.PeerPathID[:])
	spec.Attempt, _ = clievent.NewPeerAttemptID(subject.AttemptID[:])
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
	if count != 1 {
		return clievent.NativeConnectivityObserved{}, clievent.ErrInvalidEvent
	}
	if socket := value.Socket; socket != nil {
		spec.Kind = string(socket.Kind)
		spec.At = socket.At
		spec.Socket = &clievent.NativeSocketFacts{Local: socket.Local, Server: socket.Server, Duration: socket.Duration, Result: socket.Result}
	}
	if admission := value.Admission; admission != nil {
		if admission.Active < 0 || admission.Queued < 0 {
			return clievent.NativeConnectivityObserved{}, clievent.ErrInvalidEvent
		}
		spec.Kind = "admission_" + string(admission.Kind)
		spec.At = admission.At
		spec.Admission = &clievent.NativeAdmissionFacts{Wait: admission.Wait, Active: uint64(admission.Active), Queued: uint64(admission.Queued), StartsRemaining: admission.StartsRemaining, STUNRemaining: admission.STUNRemaining, ActiveTimeRemaining: admission.ActiveTimeRemaining}
	}
	if lifecycle := value.Lifecycle; lifecycle != nil {
		spec.Kind = string(lifecycle.Kind)
		spec.At = lifecycle.At
		spec.Lifecycle = &clievent.NativeLifecycleFacts{Content: lifecycle.Content, Direct: lifecycle.Direct, PreviousGeneration: lifecycle.PreviousGeneration}
	}
	if p := value.Provider; p != nil {
		spec.Kind = p.Milestone
		spec.At = p.At
		// Raw provider state may be an error message for setup failures. Only
		// finite transport states are facts suitable for the shared event schema.
		if slices.Contains([]string{"new", "checking", "connected", "completed", "disconnected", "failed", "closed", "connecting"}, p.State) {
			spec.State = p.State
		}
		if c := p.Candidate; c != nil {
			spec.Candidate = &clievent.NativeCandidateFacts{Priority: c.Priority, TCPType: c.TCPType, Type: c.Type, Protocol: c.Protocol, Address: nativeObservedAddress(c.Address), Port: c.Port, Family: c.Family, Origin: c.Origin}
		}
		if pair := p.Pair; pair != nil {
			spec.Pair = &clievent.NativePairFacts{LocalType: pair.LocalType, RemoteType: pair.RemoteType, Protocol: pair.Protocol, LocalAddress: nativeObservedAddress(pair.LocalAddress), RemoteAddress: nativeObservedAddress(pair.RemoteAddress), LocalPort: pair.LocalPort, RemotePort: pair.RemotePort, PairRTT: pair.RoundTripTime}
		}
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
		spec.Reachability = &clievent.NativeReachabilityFacts{Local: r.Endpoint.Local, Remote: r.Scope.Remote, Protocol: protocol, Reason: nativeReachabilityReason(r.Error), ServerEpoch: r.ServerEpoch, ServerRestarted: r.ServerRestarted}
	}
	return clievent.NewNativeConnectivityObserved(spec)
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
