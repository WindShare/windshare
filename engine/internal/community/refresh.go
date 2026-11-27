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

type refreshCycle struct {
	done  chan struct{}
	scope Scope
}

// advanceScope records a newer non-empty scope without letting a delayed
// caller roll the network generation backwards.
func (c *refreshCycle) advanceScope(next Scope) {
	if next.Origin == "" {
		return
	}
	if c.scope.Origin == "" || next.Generation > c.scope.Generation || (next.Generation == c.scope.Generation && next.Origin != c.scope.Origin) {
		c.scope = next
	}
}

// Refresh coalesces startup bootstrapping and periodic pool maintenance.
// Shared I/O runs on the pool lifetime context so caller cancellation only
// unblocks that caller's wait without poisoning coalesced refreshers or probes.
func (p *Pool) Refresh(ctx context.Context, scope Scope, seeds []string) {
	if p == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	running := p.cycle
	if running != nil {
		running.advanceScope(scope)
		p.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-p.ctx.Done():
		case <-running.done:
		}
		return
	}
	running = &refreshCycle{done: make(chan struct{}), scope: scope}
	p.cycle = running
	cursor := p.cursor
	p.cursor += fanout
	p.workers.Add(1)
	p.mu.Unlock()
	clonedSeeds := slices.Clone(seeds)
	go func() {
		defer p.workers.Done()
		defer func() {
			p.mu.Lock()
			p.cycle = nil
			close(running.done)
			p.mu.Unlock()
		}()
		p.runRefresh(running, cursor, clonedSeeds)
	}()
	select {
	case <-ctx.Done():
	case <-p.ctx.Done():
	case <-running.done:
	}
}

func (p *Pool) runRefresh(cycle *refreshCycle, cursor int, seeds []string) {
	records := p.records()
	cached := make([]string, 0, len(records))
	for _, entry := range records {
		cached = append(cached, entry.d.RelayBaseURL())
	}
	var initial sync.WaitGroup
	var warmedScope Scope
	if len(records) > 0 {
		p.mu.Lock()
		warmedScope = cycle.scope
		p.mu.Unlock()
		initial.Go(func() { p.warm(p.ctx, warmedScope, cursor) })
	}
	pull := func(targets []string) bool {
		ready := false
		for i := 0; i < min(fanout, len(targets)); i++ {
			if p.ctx.Err() != nil {
				return ready
			}
			target := targets[(cursor+i)%len(targets)]
			if err := p.sample(p.ctx, target); err == nil {
				ready = true
			} else if p.ctx.Err() == nil {
				p.trace("community_pull_failed", target, err.Error())
			}
		}
		return ready
	}
	if !pull(cached) && p.ctx.Err() == nil {
		pull(seeds)
	}
	records = p.records()
	if p.path != "" && len(records) > 0 && p.ctx.Err() == nil {
		raw := make([]json.RawMessage, 0, len(records))
		for _, r := range records {
			raw = append(raw, r.raw)
		}
		data, _ := json.Marshal(raw)
		if err := statefile.Write(p.ctx, p.path, data); err != nil && p.ctx.Err() == nil {
			p.trace("community_cache_failed", p.path, err.Error())
		}
	}
	if len(cached) == 0 && p.ctx.Err() == nil {
		p.mu.Lock()
		warmedScope = cycle.scope
		p.mu.Unlock()
		p.warm(p.ctx, warmedScope, cursor)
	}
	initial.Wait()
	// A coalesced caller may arrive mid-cycle with a newer frontend origin or
	// network generation. Re-warm once for that latest scope so callers do not
	// sleep for RefreshInterval with observations bound only to the old scope.
	p.mu.Lock()
	latestScope := cycle.scope
	p.mu.Unlock()
	if latestScope.Origin != "" && latestScope != warmedScope && p.ctx.Err() == nil {
		p.warm(p.ctx, latestScope, cursor)
	}
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
			if err := p.check(ctx, scope, entry.d.RelayEndpoint, RefreshInterval); err != nil && ctx.Err() == nil {
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
	if p == nil {
		return serviceauthority.ErrExpired
	}
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
	if p == nil {
		return serviceauthority.ErrExpired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	job := probeKey{scope, identity}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return p.ctx.Err()
	}
	entry, ok := p.peers[identity]
	if !ok || int64(entry.d.ExpiresAt) <= p.now().Unix() {
		p.mu.Unlock()
		return serviceauthority.ErrExpired
	}
	if entry.live.scope == scope && entry.live.until.After(p.now().Add(minValidity)) && (entry.d.PublicRelay != serviceauthority.RelayAnonymous || entry.live.relay) {
		p.mu.Unlock()
		return nil
	}
	running := p.inflight[job]
	if running == nil {
		running = &probe{done: make(chan struct{})}
		p.inflight[job] = running
		p.workers.Add(1)
		p.mu.Unlock()
		go p.runProbe(job, entry, running)
	} else {
		p.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return p.ctx.Err()
	case <-running.done:
		return running.err
	}
}

func (p *Pool) runProbe(job probeKey, entry peer, running *probe) {
	defer p.workers.Done()
	select {
	case p.probes <- struct{}{}:
		running.err = p.verify(p.ctx, job.scope, entry)
		<-p.probes
	case <-p.ctx.Done():
		running.err = p.ctx.Err()
	}
	p.mu.Lock()
	if running.err != nil && p.ctx.Err() == nil {
		if latest, ok := p.peers[job.identity]; ok && latest.d.NodeKey == entry.d.NodeKey && latest.live.scope == job.scope {
			latest.live = observation{}
			p.peers[job.identity] = latest
		}
	}
	delete(p.inflight, job)
	close(running.done)
	p.mu.Unlock()
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
