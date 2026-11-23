package pionpeer

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/stun/v4"
	pion "github.com/pion/webrtc/v4"
)

const hostSTUNTimeout = 5 * time.Second

type hostPortDiscovery interface {
	DiscoverXORMappedAddr(context.Context, netip.AddrPort, *net.UDPAddr, time.Duration) (netip.AddrPort, error)
}

type candidateEvent struct {
	candidate *pion.ICECandidate
	mapping   *MappedEndpoint
	probeDone bool
}

// A single consumer orders host, router and STUN candidates. Pion callbacks
// only append to the mailbox, even when DNS, a probe or the consumer stalls.
type candidateDispatch struct {
	ctx   context.Context
	mu    sync.Mutex
	queue []candidateEvent
	wake  chan struct{}
}

func (d *candidateDispatch) post(event candidateEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() != nil {
		return
	}
	d.queue = append(d.queue, event)
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (c *Connection) OnICECandidate(callback func(*pion.ICECandidate)) {
	c.candidateMu.Lock()
	defer c.candidateMu.Unlock()
	if c.closed {
		return
	}
	if c.stopCandidates != nil {
		c.stopCandidates()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.stopCandidates = cancel
	dispatch := &candidateDispatch{ctx: ctx, wake: make(chan struct{}, 1)}
	go dispatch.run(c, callback)
	c.PeerConnection.OnICECandidate(func(candidate *pion.ICECandidate) {
		dispatch.post(candidateEvent{candidate: candidate})
	})
}

func (d *candidateDispatch) run(c *Connection, callback func(*pion.ICECandidate)) {
	emit := func(candidate *pion.ICECandidate) {
		c.observeCandidate(candidate)
		if callback != nil {
			callback(candidate)
		}
	}
	seen := make(map[MappedEndpoint]bool)
	emitMapping := func(mapping MappedEndpoint, host *pion.ICECandidate) {
		if seen[mapping] {
			return
		}
		if candidate, ok := synthesizeMappedCandidate(mapping, host.SDPMid, host.SDPMLineIndex); ok {
			seen[mapping] = true
			emit(candidate)
		}
	}
	var pending int
	var complete bool
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-d.wake:
		}
		d.mu.Lock()
		batch := d.queue
		d.queue = nil
		d.mu.Unlock()
		for _, event := range batch {
			if d.ctx.Err() != nil {
				return
			}
			host := event.candidate
			switch {
			case event.probeDone:
				pending--
			case event.mapping != nil:
				emitMapping(*event.mapping, host)
			case host == nil:
				complete = true
			default:
				emit(host)
				pending += d.gatherHost(c, host, emitMapping)
			}
		}
		if complete && pending == 0 {
			emit(nil)
			return
		}
	}
}

func (d *candidateDispatch) gatherHost(c *Connection, host *pion.ICECandidate, emitMapping func(MappedEndpoint, *pion.ICECandidate)) int {
	var pending int
	ip, err := netip.ParseAddr(host.Address)
	if host.Typ != pion.ICECandidateTypeHost || err != nil {
		return 0
	}
	local := netip.AddrPortFrom(ip.Unmap(), host.Port)
	for _, mapping := range c.mapped {
		if mapping.Local == local && mapping.Protocol == host.Protocol.String() {
			emitMapping(mapping, host)
		}
	}
	if c.mux != nil && host.Protocol == pion.ICEProtocolUDP {
		for _, url := range c.request.ICE.DiscoveryURLs() {
			pending++
			go d.probe(c, local, host, url)
		}
	}
	return pending
}

func (d *candidateDispatch) probe(c *Connection, local netip.AddrPort, host *pion.ICECandidate, url string) {
	defer d.post(candidateEvent{probeDone: true})
	ctx, cancel := context.WithTimeout(d.ctx, hostSTUNTimeout)
	defer cancel()
	uri, err := stun.ParseURI(url)
	if err != nil {
		return
	}
	family := "ip6"
	if local.Addr().Is4() {
		family = "ip4"
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, family, uri.Host)
	if err == nil {
		for _, address := range addresses {
			server := net.UDPAddrFromAddrPort(netip.AddrPortFrom(address.Unmap(), uint16(uri.Port)))
			external, err := c.mux.DiscoverXORMappedAddr(ctx, local, server, hostSTUNTimeout)
			if err == nil && ctx.Err() == nil {
				mapping := MappedEndpoint{Local: local, External: external, Protocol: "udp"}
				d.post(candidateEvent{candidate: host, mapping: &mapping})
				c.observe("stun_host_port_discovered", family)
				return
			}
		}
	}
	if d.ctx.Err() == nil {
		c.observe("stun_host_port_failed", family)
	}
}
