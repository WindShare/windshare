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
	var named, literal []Candidate
	for _, c := range peers {
		ep, err := networkendpoint.NormalizeRelay(c.URL)
		if err != nil || seen[ep.IdentityURL] || c.Source != Community {
			continue
		}
		seen[ep.IdentityURL] = true
		c.Identity = ep.IdentityURL
		fact := order.facts[c.Identity]
		if !fact.CooldownUntil.IsZero() || !fact.ServerRetryAt.IsZero() {
			p.Decision.Filters = append(p.Decision.Filters, Filter{c.URL, "community_cooldown"})
			continue
		}
		host, _ := url.Parse(ep.IdentityURL)
		_, err = netip.ParseAddr(host.Hostname())
		c.IPLiteral = err == nil
		c.Alias, c.Zone, c.Operator, c.Provider = "", "", "", ""
		if c.IPLiteral {
			literal = append(literal, c)
		} else {
			named = append(named, c)
		}
	}
	order.rankCommunity(named)
	order.rankCommunity(literal)
	if len(named)+len(literal) == 0 {
		return
	}
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
	p.Standby = append(p.Standby, deferred...)
}

func (order selectionOrder) rankCommunity(group []Candidate) {
	sort.SliceStable(group, func(i, j int) bool {
		a, b := group[i], group[j]
		if a.Verified != b.Verified {
			return a.Verified
		}
		af, bf := order.facts[a.Identity], order.facts[b.Identity]
		if af.Admitted != bf.Admitted {
			return af.Admitted
		}
		if af.Admitted && af.Duration/DurationBucket != bf.Duration/DurationBucket {
			return af.Duration/DurationBucket < bf.Duration/DurationBucket
		}
		if a.CapacityMbps != b.CapacityMbps {
			return a.CapacityMbps > b.CapacityMbps
		}
		return a.Identity < b.Identity
	})
}
