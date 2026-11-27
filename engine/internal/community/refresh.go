package community

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/statefile"
)

func (p *Pool) Refresh(ctx context.Context, scope Scope, seeds []string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if running := p.cycle; running != nil {
		p.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-running:
		}
		return
	}
	p.cycle = make(chan struct{})
	cursor := p.cursor
	p.cursor += fanout
	p.mu.Unlock()
	defer func() { p.mu.Lock(); close(p.cycle); p.cycle = nil; p.mu.Unlock() }()
	var cached []string
	records := p.records()
	for _, entry := range records {
		cached = append(cached, entry.d.RelayBaseURL())
	}
	var initial sync.WaitGroup
	if len(records) > 0 {
		initial.Go(func() { p.warm(ctx, scope, cursor) })
	}
	pull := func(targets []string) bool {
		ready := false
		for i := 0; i < min(fanout, len(targets)); i++ {
			target := targets[(cursor+i)%len(targets)]
			if err := p.sample(ctx, target); err == nil {
				ready = true
			} else {
				p.trace("community_pull_failed", target, err.Error())
			}
		}
		return ready
	}
	if !pull(cached) && ctx.Err() == nil {
		pull(seeds)
	}
	records = p.records()
	if p.path != "" && len(records) > 0 {
		raw := make([]json.RawMessage, 0, len(records))
		for _, r := range records {
			raw = append(raw, r.raw)
		}
		data, _ := json.Marshal(raw)
		if err := statefile.Write(ctx, p.path, data); err != nil {
			p.trace("community_cache_failed", p.path, err.Error())
		}
	}
	if len(cached) == 0 {
		p.warm(ctx, scope, cursor)
	}
	initial.Wait()
}

func (p *Pool) warm(ctx context.Context, scope Scope, cursor int) {
	if scope.Origin == "" {
		return
	}
	records := p.records()
	// Prefer names for the supplemental slot. Rotate within each tier so a
	// stale peer cannot monopolize the bounded startup probes.
	var candidates []peer
	for i := range records {
		entry := records[(cursor+i)%len(records)]
		if entry.d.Authority == nil && entry.d.PublicRelay != serviceauthority.RelayDisabled && (entry.d.Services.STUN || entry.d.PublicRelay == serviceauthority.RelayAnonymous) {
			candidates = append(candidates, entry)
		}
	}
	isLiteral := func(x peer) bool {
		u, _ := url.Parse(x.d.RelayEndpoint)
		_, err := netip.ParseAddr(u.Hostname())
		return err == nil
	}
	slices.SortStableFunc(candidates, func(a, b peer) int {
		ra, rb := a.d.PublicRelay == serviceauthority.RelayAnonymous, b.d.PublicRelay == serviceauthority.RelayAnonymous
		if ra != rb {
			if ra {
				return -1
			}
			return 1
		}
		if isLiteral(a) == isLiteral(b) {
			return 0
		}
		if isLiteral(a) {
			return 1
		}
		return -1
	})
	var workers sync.WaitGroup
	for _, entry := range candidates[:min(fanout, len(candidates))] {
		workers.Go(func() {
			if err := p.check(ctx, scope, entry.d.RelayEndpoint, RefreshInterval); err != nil {
				p.trace("community_probe_failed", entry.d.RelayEndpoint, err.Error())
			}
		})
	}
	workers.Wait()
}

type probeKey struct {
	scope    Scope
	identity string
}
type probe struct {
	done chan struct{}
	err  error
}

func (p *Pool) Verify(ctx context.Context, scope Scope, identity string) error {
	if err := p.check(ctx, scope, identity, 0); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.peers[identity]
	if entry.d.Authority != nil || entry.d.PublicRelay != serviceauthority.RelayAnonymous || !entry.live.relay || entry.live.scope != scope || !entry.live.until.After(p.now()) {
		return errors.New("anonymous relay unavailable")
	}
	return nil
}

func (p *Pool) check(ctx context.Context, scope Scope, identity string, minValidity time.Duration) error {
	job := probeKey{scope, identity}
	p.mu.Lock()
	entry, ok := p.peers[identity]
	if !ok || int64(entry.d.ExpiresAt) <= p.now().Unix() {
		p.mu.Unlock()
		return serviceauthority.ErrExpired
	}
	if entry.live.scope == scope && entry.live.until.After(p.now().Add(minValidity)) && (entry.d.PublicRelay != serviceauthority.RelayAnonymous || entry.live.relay) {
		p.mu.Unlock()
		return nil
	}
	if running := p.inflight[job]; running != nil {
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-running.done:
			return running.err
		}
	}
	running := &probe{done: make(chan struct{})}
	p.inflight[job] = running
	p.mu.Unlock()
	select {
	case p.probes <- struct{}{}:
		running.err = p.verify(ctx, scope, entry)
		<-p.probes
	case <-ctx.Done():
		running.err = ctx.Err()
	}
	p.mu.Lock()
	if running.err != nil && ctx.Err() == nil {
		if latest, ok := p.peers[identity]; ok && latest.d.NodeKey == entry.d.NodeKey && latest.live.scope == scope {
			latest.live = observation{}
			p.peers[identity] = latest
		}
	}
	delete(p.inflight, job)
	close(running.done)
	p.mu.Unlock()
	return running.err
}

func publicPrefix(ip netip.Addr) netip.Prefix {
	bits := 48
	if ip.Is4() {
		bits = 24
	}
	return netip.PrefixFrom(ip, bits).Masked()
}

func (p *Pool) Transport() *http.Client { return p.http }

func (p *Pool) STUN(scope Scope) []icepolicy.Endpoint {
	if p == nil {
		return nil
	}
	var out []icepolicy.Endpoint
	for _, r := range p.records() {
		v := r.live
		if v.scope != scope || !v.until.After(p.now()) || !p.policy.Permits(v.ip) {
			continue
		}
		family := "ipv6"
		if v.ip.Is4() {
			family = "ipv4"
		}
		for _, raw := range v.stun {
			out = append(out, icepolicy.Endpoint{ID: "community:" + raw, URL: raw, Trust: "community", FailureDomain: publicPrefix(v.ip).String(), Family: family, Enabled: true})
		}
	}
	return out
}
