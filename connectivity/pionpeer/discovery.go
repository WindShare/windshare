package pionpeer

import (
	"net/netip"

	pion "github.com/pion/webrtc/v4"
)

type hostProbeResult struct {
	host    *pion.ICECandidate
	mapping MappedEndpoint
	found   bool
}

func (s *candidateSubscription) handle(event candidateEvent) {
	switch event.kind {
	case gatheredCandidate:
		s.gather(event.candidate)
		return
	case lateCandidate:
		// Pion pairs an active TCP host internally when a remote passive
		// candidate arrives. The SDP may already have ended wire trickle.
		s.observe(func() { s.connection.observeCandidate(event.candidate) })
		return
	case transferredTrickleEnd:
		s.endTrickle()
		return
	case hostProbeCompleted:
		s.pending--
		s.recordProbe(event.probe)
	case pionGatheringEnded:
		s.pionEnded = true
	}
	if s.pionEnded && s.pending == 0 {
		if !s.profileUncertain {
			s.observe(func() { s.connection.observeCandidate(nil) })
		}
		s.endTrickle()
	}
}

func (s *candidateSubscription) emit(candidate *pion.ICECandidate) {
	if s.observe(func() { s.connection.observeCandidate(candidate) }) && s.callback != nil {
		s.callback(candidate)
	}
}

func (s *candidateSubscription) gather(candidate *pion.ICECandidate) {
	// The application owns the candidate passed to its callback. Keep a snapshot
	// for outstanding probes and for router mappings emitted after that callback.
	host := *candidate
	ip, err := netip.ParseAddr(host.Address)
	if host.Typ != pion.ICECandidateTypeHost || err != nil {
		s.emit(candidate)
		return
	}
	local := netip.AddrPortFrom(ip.Unmap(), host.Port)
	if host.Protocol == pion.ICEProtocolUDP && s.connection.mux != nil {
		s.startProbes(local, &host)
	}
	s.emit(candidate)
	for _, mapping := range s.connection.mapped {
		if mapping.Local == local && mapping.Protocol == host.Protocol.String() {
			s.emitMapping(mapping, &host)
		}
	}
}

func (s *candidateSubscription) startProbes(local netip.AddrPort, host *pion.ICECandidate) {
	c := s.connection
	d := &c.candidates
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.current != s {
		return
	}
	// Admission shares the handoff lock so Wait can never race a new probe.
	for _, server := range c.request.ICE.DiscoveryServers() {
		s.pending++
		s.probes.Go(func() {
			external, err := discoverHostSTUN(s.ctx, c.mux, local, server, c.request.allowLoopback)
			result := &hostProbeResult{host: host, mapping: MappedEndpoint{Local: local, External: external, Protocol: "udp"}, found: err == nil}
			d.mu.Lock()
			if d.current == s {
				d.enqueueLocked(candidateEvent{kind: hostProbeCompleted, probe: result})
			}
			d.mu.Unlock()
		})
	}
}

func (s *candidateSubscription) recordProbe(result *hostProbeResult) {
	family := hostSTUNFamily(result.mapping.Local.Addr())
	if !result.found {
		s.observe(func() { s.connection.observe("stun_host_port_failed", family) })
		return
	}
	s.profileUncertain = false
	// A matching router mapping suppresses a second advertisement. STUN still
	// succeeded on this socket, and profile accounting precedes gathering completion.
	s.observe(func() { s.connection.observe(HostSTUNDiscovered, family) })
	s.emitMapping(result.mapping, result.host)
}

func (s *candidateSubscription) emitMapping(mapping MappedEndpoint, host *pion.ICECandidate) {
	if !s.seen[mapping] {
		if candidate, ok := synthesizeMappedCandidate(mapping, host.SDPMid, host.SDPMLineIndex); ok {
			s.seen[mapping] = true
			s.emit(candidate)
		}
	}
}
