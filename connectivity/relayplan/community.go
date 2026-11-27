package relayplan

import (
	"net/netip"
	"net/url"
	"slices"
	"sort"

	"github.com/windshare/windshare/core/networkendpoint"
)

func activeAlternates(all, selected []Candidate, limit int) []Candidate {
	var rest []Candidate
	for _, c := range all {
		if !slices.ContainsFunc(selected, func(s Candidate) bool { return s.Identity == c.Identity }) {
			rest = append(rest, c)
		}
	}
	var alternatives []Candidate
	for _, slot := range selected {
		i := slices.IndexFunc(rest, func(c Candidate) bool { return c.Zone == slot.Zone })
		if i >= 0 && len(alternatives) < limit {
			alternatives = append(alternatives, rest[i])
			rest = slices.Delete(rest, i, i+1)
		}
	}
	return append(alternatives, rest[:min(len(rest), limit-len(alternatives))]...)
}

// Starting a known-good community route alongside the official pair must not
// let self-reported zone, provider or capacity influence selection of that pair.
func (p *Plan) addCommunity(peers []Candidate, limit int, seen map[string]bool, order selectionOrder) {
	named, literal, cooled := classifyCommunity(peers, seen, order)
	if len(named)+len(literal) == 0 {
		p.addCooledCommunityFallback(cooled, limit, order)
		return
	}
	p.filterCooledCommunity(cooled)
	order.rankCommunity(named)
	order.rankCommunity(literal)
	// A server-requested backoff cannot report an initial handshake failure
	// until its deadline. Keep those routes behind dialable fallback waves.
	var dialable, deferred []Candidate
	for _, c := range p.Network {
		if !c.ServerRetryAt.IsZero() || (p.Decision.CooldownFallback && len(dialable) >= TargetNetworkRegistrations) {
			deferred = append(deferred, c)
		} else {
			dialable = append(dialable, c)
		}
	}
	p.Network = dialable
	if len(p.Network) > 0 && len(named) > 0 && named[0].Verified && len(p.Network) < limit {
		p.Network = append(p.Network, named[0])
		named = named[1:]
	}
	p.appendCommunityWaves(named, literal, limit)
	p.Standby = append(p.Standby, deferred...)
}

func classifyCommunity(peers []Candidate, seen map[string]bool, order selectionOrder) (named, literal, cooled []Candidate) {
	for _, c := range peers {
		ep, err := networkendpoint.NormalizeRelay(c.URL)
		if err != nil || seen[ep.IdentityURL] || c.Source != Community {
			continue
		}
		seen[ep.IdentityURL] = true
		c.Identity = ep.IdentityURL
		c = normalizeCommunityCandidate(c, ep.IdentityURL, order)
		if !c.CooldownUntil.IsZero() || !c.ServerRetryAt.IsZero() {
			cooled = append(cooled, c)
			continue
		}
		if c.IPLiteral {
			literal = append(literal, c)
		} else {
			named = append(named, c)
		}
	}
	return named, literal, cooled
}

func normalizeCommunityCandidate(c Candidate, identity string, order selectionOrder) Candidate {
	fact := order.applyHistory(&c)
	if c.ProbeFailed && c.history == unmeasured {
		c.history = recentlyFailed
	}
	cooldownUntil := fact.CooldownUntil
	if !c.CooldownUntil.IsZero() && (order.now.IsZero() || c.CooldownUntil.After(order.now)) && c.CooldownUntil.After(cooldownUntil) {
		cooldownUntil = c.CooldownUntil
	}
	c.CooldownUntil, c.ServerRetryAt = cooldownUntil, fact.ServerRetryAt
	host, _ := url.Parse(identity)
	_, err := netip.ParseAddr(host.Hostname())
	c.IPLiteral = err == nil
	c.Alias, c.Zone, c.Operator, c.Provider = "", "", "", ""
	return c
}

func (p *Plan) addCooledCommunityFallback(cooled []Candidate, limit int, order selectionOrder) {
	// Only fall back to cooled community peers when no configured, official,
	// or uncooled community route exists—avoiding a NoRelayCandidates lockout
	// on a transient hiccup when retrying the sole cached community peer.
	if len(cooled) == 0 || len(p.Configured) > 0 || len(p.Network) > 0 || len(p.Standby) > 0 {
		p.filterCooledCommunity(cooled)
		return
	}
	p.Decision.CooldownFallback = true
	var cooledNamed, cooledLiteral []Candidate
	for _, c := range cooled {
		if c.IPLiteral {
			cooledLiteral = append(cooledLiteral, c)
		} else {
			cooledNamed = append(cooledNamed, c)
		}
	}
	order.rankCooledCommunity(cooledNamed)
	order.rankCooledCommunity(cooledLiteral)
	p.appendCommunityWaves(cooledNamed, cooledLiteral, limit)
}

func (p *Plan) filterCooledCommunity(cooled []Candidate) {
	for _, c := range cooled {
		p.Decision.Filters = append(p.Decision.Filters, Filter{c.URL, "community_cooldown"})
	}
}

func (p *Plan) appendCommunityWaves(named, literal []Candidate, limit int) {
	for _, group := range [][]Candidate{named, literal} {
		if len(group) == 0 {
			continue
		}
		group = slices.Clone(group[:min(len(group), TargetNetworkRegistrations, limit)])
		if len(p.Network) == 0 {
			p.Network = group
		} else {
			p.Standby = append(p.Standby, group...)
		}
	}
}

func (order selectionOrder) communityLess(a, b Candidate) bool {
	if a.Verified != b.Verified {
		return a.Verified
	}
	if a.history != b.history {
		return a.history > b.history
	}
	if a.history == recentlyAdmitted {
		if l, r := max(a.ObservedDuration, 0)/DurationBucket, max(b.ObservedDuration, 0)/DurationBucket; l != r {
			return l < r
		}
	}
	if a.CapacityMbps != b.CapacityMbps {
		return a.CapacityMbps > b.CapacityMbps
	}
	return order.tieBreak(a, b)
}

func (order selectionOrder) rankCommunity(group []Candidate) {
	sort.SliceStable(group, func(i, j int) bool {
		return order.communityLess(group[i], group[j])
	})
}

func (order selectionOrder) rankCooledCommunity(group []Candidate) {
	sort.SliceStable(group, func(i, j int) bool {
		if cmp := order.retryPriority(group[i], group[j]); cmp != 0 {
			return cmp < 0
		}
		return order.communityLess(group[i], group[j])
	})
}
