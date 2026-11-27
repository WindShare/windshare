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
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/statefile"
)

// Seeds groups untrusted bootstrap origins into ordered fallback tiers.
// Local cached peers are always consulted before any bootstrap tier.
type Seeds struct {
	Configured []string
	Active     []string
	Fallback   []string
}

func (s Seeds) clone() Seeds {
	return Seeds{
		Configured: slices.Clone(s.Configured),
		Active:     slices.Clone(s.Active),
		Fallback:   slices.Clone(s.Fallback),
	}
}

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
func (p *Pool) Refresh(ctx context.Context, scope Scope, seeds Seeds) {
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
	clonedSeeds := seeds.clone()
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

func (p *Pool) runRefresh(cycle *refreshCycle, cursor int, seeds Seeds) {
	records := p.records()
	cached := make([]string, 0, len(records))
	for _, entry := range records {
		cached = append(cached, entry.d.RelayBaseURL())
	}
	var initial sync.WaitGroup
	var initialAttempted []string
	var warmedScope Scope
	if len(records) > 0 {
		p.mu.Lock()
		warmedScope = cycle.scope
		p.mu.Unlock()
		initial.Go(func() { initialAttempted = p.warm(p.ctx, warmedScope, cursor, fanout, nil) })
	}
	pulled := p.pull(cursor, cached, seeds)
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
	initial.Wait()
	if p.ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	currentScope := cycle.scope
	p.mu.Unlock()
	if warmedScope.Origin == "" {
		warmedScope = currentScope
		if warmedScope.Origin != "" {
			p.warm(p.ctx, warmedScope, cursor, fanout, nil)
		}
	} else if warmedScope == currentScope {
		// Stale caches may fail initial warming before pull discovers healthy
		// peers from seeds; fill remaining fanout slots once without re-probing
		// peers already attempted in this cycle or amplifying offline probes.
		verified := p.verifiedCount(warmedScope)
		if verified < fanout && (pulled || verified > 0) {
			exclude := make(map[string]struct{}, len(initialAttempted))
			for _, id := range initialAttempted {
				exclude[id] = struct{}{}
			}
			p.warm(p.ctx, warmedScope, cursor, fanout-verified, exclude)
		}
	}
	// A coalesced caller may arrive mid-cycle with a newer frontend origin or
	// network generation. Re-warm once for that latest scope so callers do not
	// sleep for RefreshInterval with observations bound only to the old scope.
	p.mu.Lock()
	latestScope := cycle.scope
	p.mu.Unlock()
	if latestScope.Origin != "" && latestScope != warmedScope && p.ctx.Err() == nil {
		p.warm(p.ctx, latestScope, cursor, fanout, nil)
	}
}

func (p *Pool) pull(cursor int, cached []string, seeds Seeds) bool {
	tried := make(map[string]struct{}, fanout+maxSeedAttempts)
	if len(cached) > 0 && p.ctx.Err() == nil {
		if p.pullBatch(p.ctx, p.nextTargets(cached, cursor, fanout, tried)) {
			return true
		}
	}
	if p.ctx.Err() != nil {
		return false
	}
	return p.pullSeeds(cursor, seeds, tried)
}

// pullSeeds walks bootstrap tiers in strict preference order and advances past
// unreachable seeds within per-tier, total-attempt, and wall-clock caps so a
// dead tier or offline network cannot stall recovery indefinitely.
func (p *Pool) pullSeeds(cursor int, seeds Seeds, tried map[string]struct{}) bool {
	tiers := [][]string{seeds.Configured, seeds.Active, seeds.Fallback}
	if !slices.ContainsFunc(tiers, func(t []string) bool { return len(t) > 0 }) {
		return false
	}
	budget := p.seedPullBudget
	if budget <= 0 {
		budget = defaultSeedPullBudget
	}
	seedCtx, cancel := context.WithTimeout(p.ctx, budget)
	defer cancel()
	attempts := 0
	for _, tier := range tiers {
		tierAttempts := 0
		for len(tier) > 0 && attempts < maxSeedAttempts && tierAttempts < maxTierAttempts && seedCtx.Err() == nil {
			limit := min(fanout, maxSeedAttempts-attempts, maxTierAttempts-tierAttempts)
			batch := p.nextTargets(tier, cursor, limit, tried)
			if len(batch) == 0 {
				break
			}
			attempts += len(batch)
			tierAttempts += len(batch)
			if p.pullBatch(seedCtx, batch) {
				return true
			}
		}
	}
	return false
}

func (p *Pool) targetKey(raw string) string {
	if origin, err := p.policy.RelayOrigin(raw); err == nil {
		return origin.String()
	}
	return raw
}

func (p *Pool) nextTargets(targets []string, start, limit int, tried map[string]struct{}) []string {
	if len(targets) == 0 || limit <= 0 {
		return nil
	}
	out := make([]string, 0, min(limit, len(targets)))
	n := len(targets)
	offset := ((start % n) + n) % n
	for i := 0; i < n && len(out) < limit; i++ {
		target := targets[(offset+i)%n]
		key := p.targetKey(target)
		if _, seen := tried[key]; seen {
			continue
		}
		tried[key] = struct{}{}
		out = append(out, target)
	}
	return out
}

func (p *Pool) pullBatch(ctx context.Context, targets []string) bool {
	if len(targets) == 0 || ctx.Err() != nil {
		return false
	}
	if len(targets) == 1 {
		if err := p.sample(ctx, targets[0]); err == nil {
			return true
		} else if p.ctx.Err() == nil {
			p.trace("community_pull_failed", targets[0], err.Error())
		}
		return false
	}
	var ready atomic.Bool
	var workers sync.WaitGroup
	for _, target := range targets {
		workers.Go(func() {
			if err := p.sample(ctx, target); err == nil {
				ready.Store(true)
			} else if p.ctx.Err() == nil {
				p.trace("community_pull_failed", target, err.Error())
			}
		})
	}
	workers.Wait()
	return ready.Load()
}

func warmEligible(entry peer) bool {
	return entry.d.OffersCommunityService()
}

func warmVerified(entry peer, scope Scope, now time.Time) bool {
	if !warmEligible(entry) {
		return false
	}
	if entry.d.PublicRelay == serviceauthority.RelayAnonymous && entry.d.Services.Relay {
		return entry.relay.scope == scope && entry.relay.until.After(now)
	}
	return entry.host.generation == scope.Generation && entry.host.until.After(now)
}

func (p *Pool) verifiedCount(scope Scope) int {
	if scope.Origin == "" {
		return 0
	}
	now := p.now()
	count := 0
	for _, entry := range p.records() {
		if warmVerified(entry, scope, now) {
			count++
		}
	}
	return count
}

func (p *Pool) warm(ctx context.Context, scope Scope, cursor, limit int, exclude map[string]struct{}) []string {
	if scope.Origin == "" || limit <= 0 {
		return nil
	}
	records := p.records()
	now := p.now()
	// Prefer names for the supplemental slot. Rotate within each tier so a
	// stale peer cannot monopolize the bounded startup probes.
	var candidates []peer
	for i := range records {
		entry := records[(cursor+i)%len(records)]
		if !warmEligible(entry) || entry.warmBlocked(scope, now) {
			continue
		}
		if exclude != nil {
			if _, skip := exclude[entry.d.RelayEndpoint]; skip || warmVerified(entry, scope, now) {
				continue
			}
		}
		candidates = append(candidates, entry)
	}
	if len(candidates) == 0 {
		return nil
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
	selected := candidates[:min(limit, len(candidates))]
	attempted := make([]string, 0, len(selected))
	var workers sync.WaitGroup
	for _, entry := range selected {
		attempted = append(attempted, entry.d.RelayEndpoint)
		workers.Go(func() {
			_ = p.check(ctx, scope, entry.d.RelayEndpoint, RefreshInterval, probeLaneBackground)
		})
	}
	workers.Wait()
	return attempted
}

type probeLane uint8

const (
	probeLaneBackground probeLane = iota
	probeLaneForeground
)

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
	if err := p.check(ctx, scope, identity, 0, probeLaneForeground); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.peers[identity]
	if entry.d.Authority != nil || entry.d.PublicRelay != serviceauthority.RelayAnonymous || !entry.d.Services.Relay || entry.relay.scope != scope || !entry.relay.until.After(p.now()) {
		return errors.New("anonymous relay unavailable")
	}
	return nil
}

func (p *Pool) check(ctx context.Context, scope Scope, identity string, minValidity time.Duration, lane probeLane) error {
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
	now := p.now()
	entry, ok := p.peers[identity]
	if !ok || int64(entry.d.ExpiresAt) <= now.Unix() {
		p.mu.Unlock()
		return serviceauthority.ErrExpired
	}
	relayTarget := entry.d.PublicRelay == serviceauthority.RelayAnonymous && entry.d.Services.Relay
	fresh := entry.host.generation == scope.Generation && entry.host.until.After(now.Add(minValidity))
	if relayTarget {
		fresh = entry.relay.scope == scope && entry.relay.until.After(now.Add(minValidity))
	}
	if fresh {
		p.mu.Unlock()
		return nil
	}
	// Fast-fail policy refusals and live-signed relay unavailability so concurrent
	// plans do not re-dial a node that already refused this scope. Transient
	// errors still allow an explicit Verify when relayplan falls back to the sole
	// remaining candidate.
	if entry.d.PublicRelay == serviceauthority.RelayAnonymous && entry.relaySuppressed(now) {
		p.mu.Unlock()
		return errors.New("anonymous relay unavailable")
	}
	if b, failed := entry.failureState(scope, now); failed && b.kind == probeFailureRejected && b.until.After(now) {
		p.mu.Unlock()
		return errors.New("community verification backed off")
	}
	running := p.inflight[job]
	if running == nil {
		running = &probe{done: make(chan struct{})}
		p.inflight[job] = running
		p.workers.Add(1)
		p.mu.Unlock()
		go p.runProbe(job, entry, running, lane)
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

func (p *Pool) probeSlots(lane probeLane) chan struct{} {
	if lane == probeLaneForeground {
		return p.verifyProbes
	}
	return p.warmProbes
}

func (p *Pool) runProbe(job probeKey, entry peer, running *probe, lane probeLane) {
	defer p.workers.Done()
	slots := p.probeSlots(lane)
	select {
	case slots <- struct{}{}:
		running.err = p.verify(p.ctx, job.scope, entry)
		<-slots
	case <-p.ctx.Done():
		running.err = p.ctx.Err()
	}
	p.mu.Lock()
	if running.err != nil && p.ctx.Err() == nil {
		p.recordProbeFailureLocked(job.identity, job.scope, entry.d, running.err, p.now())
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

// STUN returns live-verified community STUN endpoints for the current network
// generation. Native UDP STUN does not depend on browser WebSocket Origin, so
// validity is keyed on network generation, descriptor lifetime, and dial policy.
func (p *Pool) STUN(generation uint64) []icepolicy.Endpoint {
	if p == nil || generation == 0 {
		return nil
	}
	now := p.now()
	var out []icepolicy.Endpoint
	for _, r := range p.records() {
		v := r.host
		if v.generation != generation || !v.until.After(now) || !p.policy.Permits(v.ip) {
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
