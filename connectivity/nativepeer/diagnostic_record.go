package nativepeer

import (
	"net/netip"
	"strconv"
	"time"

	"github.com/windshare/windshare/connectivity/pionpeer"

	"github.com/windshare/windshare/connectivity/reachability"
	diag "github.com/windshare/windshare/core/diagnostics"
)

const connectivityEventName = "native_connectivity"

func (value Observation) DiagnosticRecord() diag.Record {
	subject := value.Subject
	fields := diag.Fields{"kind": diag.Text("unknown"), "state": diag.Text("unknown"), "side": diag.Text(subject.Side),
		"attempt_sequence": knownDiagnosticUint(subject.AttemptSequence), "network_generation_id": knownDiagnosticUint(subject.NetworkGenerationID),
		"ice_profile_id": diag.Text(subject.ICEProfileID)}
	var at time.Time
	if socket := value.Socket; socket != nil {
		fields["kind"], at = diag.Text(string(socket.Kind)), socket.At
		fields["socket"] = diag.Object(diag.Fields{"local_endpoint": diagnosticEndpoint(socket.Local), "stun_server": diagnosticEndpoint(socket.Server),
			"duration_ms": diag.Millis(socket.Duration), "result": diag.Text(socket.Result)})
	}
	if lifecycle := value.Lifecycle; lifecycle != nil {
		fields["kind"], at = diag.Text(string(lifecycle.Kind)), lifecycle.At
		fields["lifecycle"] = diag.Object(diag.Fields{"content_demand": diag.Bool(lifecycle.Content), "direct_demand": diag.Bool(lifecycle.Direct),
			"previous_network_generation_id": knownDiagnosticUint(lifecycle.PreviousGeneration)})
	}
	if admission := value.Admission; admission != nil {
		fields["kind"], at = diag.Text("admission_"+string(admission.Kind)), admission.At
		a := diag.Fields{"purpose": diag.Text(admission.Purpose.String()), "wait_ms": diag.Millis(admission.Wait),
			"active": diag.Int(int64(admission.Active)), "queued": diag.Int(int64(admission.Queued)),
			"starts_remaining":         diag.Text(strconv.FormatFloat(admission.StartsRemaining, 'f', -1, 64)),
			"stun_remaining":           diag.Text(strconv.FormatFloat(admission.STUNRemaining, 'f', -1, 64)),
			"active_time_remaining_ms": diag.Millis(admission.ActiveTimeRemaining)}
		if c := admission.SocketCapacity; c != nil {
			a["socket_capacity"] = diag.Object(diag.Fields{"used": diag.Int(int64(c.Used)), "reserved": diag.Int(int64(c.Reserved)), "requested": diag.Int(int64(c.Requested)), "limit": diag.Int(int64(c.Limit))})
		}
		fields["admission"] = diag.Object(a)
	}
	if r := value.Reachability; r != nil {
		fields["kind"] = diag.Text(r.Kind)
		reason := ""
		if r.Error != nil {
			reason = r.Error.Error()
		}
		fields["reachability"] = diag.Object(diag.Fields{"local_endpoint": diagnosticEndpoint(r.Endpoint.Local), "remote_scope": diagnosticEndpoint(r.Scope.Remote),
			"protocol": diag.Text(diagnosticProtocol(r.Endpoint.Protocol)), "reason": diag.Text(reason), "server_epoch": diag.Number(float64(r.ServerEpoch)), "server_restarted": diag.Bool(r.ServerRestarted)})
	}
	if p := value.Provider; p != nil {
		fields["kind"], fields["state"], at = diag.Text(p.Milestone), diag.Text(p.State), p.At
		providerDiagnosticFields(fields, p)
	}
	if t := value.TURN; t != nil {
		fields["kind"], at = diag.Text("turn_path"), t.At
		fields["turn"] = diag.Object(diag.Fields{"provider": diag.Text(t.Provider), "account": diag.Text(t.Account), "profile": diag.Text(t.Profile),
			"milestone": diag.Text(t.Milestone.String()), "reason": diag.Text(t.Reason), "useful_bytes": diag.Uint(t.UsefulBytes)})
	}
	if p := value.Parallelism; p != nil {
		d, o := p.Decision, p.Demand
		fields["kind"], at = diag.Text("parallelism"), p.At
		optional := uint64(0)
		for _, path := range o.Paths {
			if path.Optional {
				optional++
			}
		}
		fields["parallelism"] = diag.Object(diag.Fields{
			"action": diag.Text(string(d.Action)), "reason": diag.Text(d.Reason), "paths": diag.Uint(uint64(len(o.Paths))), "optional_paths": diag.Uint(optional),
			"network_waiting_bytes": diag.Uint(o.NetworkWaitingBytes), "in_flight_bytes": diag.Uint(o.InFlightBytes), "remaining_bytes": diag.Uint(o.RemainingBytes), "remaining_known": diag.Bool(o.RemainingKnown),
			"queued_bytes": diag.Uint(o.QueuedBytes), "unfinished_bytes": diag.Uint(o.UnfinishedBytes), "capacity_waiting_bytes": diag.Uint(o.CapacityWaitingBytes),
			"useful_rate": diag.Number(d.UsefulRate), "output_rate": diag.Number(d.OutputRate), "baseline_useful_rate": diag.Number(d.BaselineUsefulRate), "baseline_output_rate": diag.Number(d.BaselineOutputRate),
			"trial_path": diag.Text(d.Path.PathID), "trial_lane_id": diag.Number(float64(d.Path.LaneID)), "trial_lane_epoch": diag.Number(float64(d.Path.LaneEpoch)),
			"offered_bytes": diag.Uint(d.Work.OfferedBytes), "settled_bytes": diag.Uint(d.Work.SettledBytes), "completed_bytes": diag.Uint(d.Work.CompletedBytes),
			"completed_attempts": diag.Uint(d.Work.CompletedAttempts), "failed_attempts": diag.Uint(d.Work.FailedAttempts),
			"superseded_attempts": diag.Uint(d.Work.SupersededAttempts), "cancelled_attempts": diag.Uint(d.Work.CancelledAttempts), "busy_ms": diag.Int(d.Work.BusyMilliseconds)})
	}
	fields["observed_at"] = diag.Text(at.UTC().Format(time.RFC3339Nano))
	c := diag.Fields{}
	if subject.ProtocolSessionID != ([16]byte{}) {
		c["protocol_session_id"] = diag.Identity(subject.ProtocolSessionID[:])
	}
	if subject.PeerPathID != ([16]byte{}) {
		c["peer_path_id"] = diag.Identity(subject.PeerPathID[:])
	}
	if subject.AttemptID != ([16]byte{}) {
		c["peer_attempt_id"] = diag.Identity(subject.AttemptID[:])
	}
	return diag.New(connectivityEventName, fields).At(at).Correlate(c)
}
func knownDiagnosticUint(v uint64) diag.Value {
	if v == 0 {
		return diag.Text("unknown")
	}
	return diag.Uint(v)
}
func diagnosticEndpoint(v netip.AddrPort) diag.Value {
	if !v.IsValid() {
		return diag.Text("unknown")
	}
	return diag.Text(v.String())
}
func diagnosticProtocol(value reachability.Protocol) string {
	switch value {
	case reachability.UDP:
		return "udp"
	case reachability.TCP:
		return "tcp"
	default:
		return "unknown"
	}
}

func diagnosticLocalRoute(address netip.Addr) diag.Value {
	if !address.IsValid() {
		return diag.Text("system")
	}
	return diag.Text(address.String())
}

func providerDiagnosticFields(fields diag.Fields, p *pionpeer.Event) {

	if d := p.Diagnostic; d != nil {
		fields["diagnostic"] = diag.Object(diag.Fields{"scope": diag.Text(d.Scope), "severity": diag.Text(d.Severity), "message": diag.Text(d.Message)})
	}
	if c := p.Candidate; c != nil {
		fields["candidate"] = diag.Object(diag.Fields{"priority": diag.Number(float64(c.Priority)), "tcp_type": diag.Text(c.TCPType),
			"type": diag.Text(c.Type), "protocol": diag.Text(c.Protocol), "address": diag.Text(c.Address), "port": diag.Number(float64(c.Port)),
			"family": diag.Text(c.Family), "origin": diag.Text(c.Origin)})
	}
	if pair := p.Pair; pair != nil {
		fields["selected_pair"] = diag.Object(diag.Fields{"local_type": diag.Text(pair.LocalType), "remote_type": diag.Text(pair.RemoteType),
			"protocol": diag.Text(pair.Protocol), "local_address": diag.Text(pair.LocalAddress), "remote_address": diag.Text(pair.RemoteAddress),
			"local_port": diag.Number(float64(pair.LocalPort)), "remote_port": diag.Number(float64(pair.RemotePort)),
			"pair_rtt_ms": diag.Millis(pair.RoundTripTime)})
	}
	if p.TURNService != nil {
		fields["turn"] = providerTURNDiagnostic(p)
	}
}

func providerTURNDiagnostic(p *pionpeer.Event) diag.Value {
	t := diag.Fields{"provider": diag.Text(p.TURNService.Provider), "account": diag.Text(p.TURNService.Account), "profile": diag.Text(p.TURNService.Profile), "reason": diag.Text(p.TURNCleanup.Reason())}
	if r := p.TURN; r != nil {
		t["resources"] = diag.Object(diag.Fields{"active_sockets": diag.Int(int64(r.ActiveSockets)), "opened_sockets": diag.Uint(r.OpenedSockets),
			"transport_sent_bytes": diag.Uint(r.TransportSent), "transport_received_bytes": diag.Uint(r.TransportReceived)})
	}
	if b := p.TURNBinding; b != nil {
		t["binding"] = diag.Object(diag.Fields{"status": diag.Text(b.Status.String()), "code": diag.Number(float64(b.Code)),
			"channel": diag.Number(float64(b.Channel)), "peer": diag.Text(b.PeerAddress), "allocation": diag.Text(b.RelayedAddress)})
	}
	if r := p.TURNRoute; r != nil {
		t["route"] = diag.Object(diag.Fields{"server": diag.Text(r.Server), "local": diagnosticLocalRoute(r.LocalAddress), "outcome": diag.Text(r.Outcome)})
	}
	return diag.Object(t)
}
