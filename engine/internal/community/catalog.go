// Package community owns the native pull-only peer cache. Signed gossip and live
// reachability have independent lifetimes and are never imported into directory trust.
package community

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/publicdial"
	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/statefile"
)

const (
	maximumPeers          = 64
	sampleSize            = 8
	fanout                = 2
	maxTierAttempts       = 4
	maxSeedAttempts       = 8
	defaultSeedPullBudget = 6 * time.Second
	RefreshInterval       = 2 * time.Minute
	requestTimeout        = 4 * time.Second
	exchangePath          = "/discovery/v1/peers"
	bytesPerMbps          = 125_000
)

type Scope struct {
	Origin     string
	Generation uint64
}
type observation struct {
	scope Scope
	relay bool
	until time.Time
	ip    netip.Addr
	stun  []string
}
type peer struct {
	raw                     json.RawMessage
	d                       serviceauthority.GossipDescriptor
	live                    observation
	relaySuppressedUntil    time.Time
	relaySuppressedIssuedAt uint64
	failures                map[Scope]probeBackoff
}
type Pool struct {
	policy         publicdial.Policy
	http           *http.Client
	path           string
	now            func() time.Time
	sink           diagnostics.Sink
	seedPullBudget time.Duration
	ctx            context.Context
	cancel         context.CancelFunc
	workers        sync.WaitGroup
	mu             sync.Mutex
	closed         bool
	peers          map[string]peer
	cycle          *refreshCycle
	cursor         int
	// Isolate background warming from foreground on-demand Verify() slots so
	// stalled background probes cannot head-of-line block share fallback, while
	// keeping warmProbes sized to fanout so coalesced foreground callers never
	// queue behind background probes.
	warmProbes   chan struct{}
	verifyProbes chan struct{}
	inflight     map[probeKey]*probe
}

func New(path string, policy publicdial.Policy, now func() time.Time, sink diagnostics.Sink) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		policy:         policy,
		http:           policy.Client(),
		now:            now,
		sink:           sink,
		seedPullBudget: defaultSeedPullBudget,
		ctx:            ctx,
		cancel:         cancel,
		peers:          make(map[string]peer),
		warmProbes:     make(chan struct{}, fanout),
		verifyProbes:   make(chan struct{}, fanout),
		inflight:       make(map[probeKey]*probe),
	}
	if path != "" {
		p.path = filepath.Join(statefile.StateDirectory(path), "community-peers.json")
		if f, err := os.Open(p.path); err == nil {
			data, err := readBounded(f, maximumPeers*(serviceauthority.MaximumDescriptorBytes+1)+2)
			_ = f.Close()
			var entries []json.RawMessage
			if err == nil && json.Unmarshal(data, &entries) == nil && len(entries) <= maximumPeers {
				for _, r := range entries {
					p.accept(r, "cache")
				}
			}
		}
	}
	return p
}

// Close aborts pool-owned refreshes and probes and joins their goroutines
// before releasing idle HTTP connections.
func (p *Pool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.closed = true
	p.cancel()
	p.mu.Unlock()
	p.workers.Wait()
	p.http.CloseIdleConnections()
}
func (p *Pool) trace(event, source, reason string) {
	if p.sink.Enabled() {
		p.sink.Emit(diagnostics.New(event, diagnostics.Fields{"source": diagnostics.Text(source), "reason": diagnostics.Text(reason)}).At(p.now()))
	}
}
func readBounded(r io.Reader, n int) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(n+1)))
	if len(b) > n {
		return nil, serviceauthority.ErrFormat
	}
	return b, err
}
func (p *Pool) accept(raw json.RawMessage, source string) bool {
	d, err := serviceauthority.InspectGossip(raw, p.now())
	if err == nil {
		_, err = p.policy.RelayOrigin(d.RelayBaseURL())
	}
	if err != nil {
		p.trace("community_descriptor_rejected", source, err.Error())
		return false
	}
	if d.PublicRelay == serviceauthority.RelayDisabled {
		p.mu.Lock()
		if old, ok := p.peers[d.RelayEndpoint]; ok && old.d.NodeKey == d.NodeKey && d.IssuedAt >= old.d.IssuedAt {
			delete(p.peers, d.RelayEndpoint)
		}
		p.mu.Unlock()
		p.trace("community_descriptor_rejected", source, "contribution_disabled")
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := d.RelayEndpoint
	old, ok := p.peers[key]
	if ok && old.d.NodeKey != d.NodeKey && old.live.until.After(p.now()) {
		p.trace("community_descriptor_rejected", source, "endpoint_bound_to_live_key")
		return false
	}
	if ok && old.d.ExpiresAt >= d.ExpiresAt {
		return old.d.NodeKey == d.NodeKey
	}
	if !ok && len(p.peers) >= maximumPeers {
		var oldest string
		for k, entry := range p.peers {
			if !entry.live.until.After(p.now()) && (oldest == "" || entry.d.ExpiresAt < p.peers[oldest].d.ExpiresAt) {
				oldest = k
			}
		}
		if oldest == "" {
			return false
		}
		delete(p.peers, oldest)
		p.trace("community_descriptor_evicted", oldest, "capacity")
	}
	if old.d.NodeKey != d.NodeKey || old.d.PublicRelay != d.PublicRelay || old.d.Services != d.Services {
		old.live = observation{}
		old.relaySuppressedUntil = time.Time{}
		old.relaySuppressedIssuedAt = 0
		old.failures = nil
	}
	// Preserve live relay suppression and scoped probe backoff across same-key
	// gossip refreshes so broken or Sybil peers cannot reset cooldowns by re-signing.
	p.peers[key] = peer{
		raw:                     bytes.Clone(raw),
		d:                       d,
		live:                    old.live,
		relaySuppressedUntil:    old.relaySuppressedUntil,
		relaySuppressedIssuedAt: old.relaySuppressedIssuedAt,
		failures:                old.failures,
	}
	p.trace("community_descriptor_accepted", source, key)
	return true
}

func (p *Pool) records() []peer {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]peer, 0, len(p.peers))
	for k, v := range p.peers {
		if int64(v.d.ExpiresAt) <= p.now().Unix() {
			delete(p.peers, k)
		} else {
			v.failures = maps.Clone(v.failures)
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b peer) int { return strings.Compare(a.d.RelayEndpoint, b.d.RelayEndpoint) })
	return out
}

func (p *Pool) Candidates(scope Scope) []relayplan.Candidate {
	if p == nil {
		return nil
	}
	now := p.now()
	var result []relayplan.Candidate
	for _, entry := range p.records() {
		d := entry.d
		if d.Authority != nil || d.PublicRelay != serviceauthority.RelayAnonymous || !d.Services.Relay {
			continue
		}
		if entry.relaySuppressed(now) {
			continue
		}
		var cooldownUntil time.Time
		failure, recentFailure := entry.failureState(scope, now)
		if recentFailure && failure.until.After(now) {
			if failure.kind == probeFailureRejected {
				continue
			}
			cooldownUntil = failure.until
		}
		u, _ := url.Parse(d.RelayEndpoint)
		_, err := netip.ParseAddr(u.Hostname())
		cap := uint32(0)
		if d.PublicCapacity != nil {
			cap = uint32(min(*d.PublicCapacity/bytesPerMbps, uint64(^uint32(0))))
		}
		result = append(result, relayplan.Candidate{
			Source:        relayplan.Community,
			URL:           d.RelayBaseURL(),
			Identity:      d.RelayEndpoint,
			Verified:      entry.live.relay && scope == entry.live.scope && entry.live.until.After(now),
			IPLiteral:     err == nil,
			CapacityMbps:  cap,
			CooldownUntil: cooldownUntil,
			ProbeFailed:   recentFailure,
		})
	}
	return result
}

func (p *Pool) sample(ctx context.Context, target string) error {
	origin, err := p.policy.RelayOrigin(target)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.JoinPath(exchangePath).String(), nil)
	if err != nil {
		return err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	data, err := readBounded(resp.Body, sampleSize*(serviceauthority.MaximumDescriptorBytes+1)+2)
	if err != nil {
		return err
	}
	var entries []json.RawMessage
	if json.Unmarshal(data, &entries) != nil || len(entries) > sampleSize {
		return serviceauthority.ErrFormat
	}
	accepted := false
	for _, entry := range entries {
		if p.accept(entry, target) {
			accepted = true
		}
	}
	if !accepted {
		return errors.New("empty or outdated discovery sample")
	}
	return nil
}
