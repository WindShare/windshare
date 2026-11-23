package turnsession

import (
	"slices"
	"time"

	p "github.com/windshare/windshare/core/session/protocolsession"
)

// OfferGrant authorizes one accepted offer while it waits for local ICE capacity.
// Later rotations change what a new offer can bind without revoking this one.
type OfferGrant struct {
	grant   p.TURNGrant
	expires time.Time
}

func (s *Sender) BindOffer(id p.TURNGrantID) (OfferGrant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.issued[id]
	if s.closed || !g.validAt(s.now()) {
		return OfferGrant{}, false
	}
	return g, true
}

func (g OfferGrant) ServiceID() string { return g.grant.ServiceID }

func (g OfferGrant) validAt(now time.Time) bool {
	return g.grant.ServiceID != "" && (g.expires.IsZero() || now.Before(g.expires))
}

// GrantAt supplies local ICE credentials. Wire TTL rounding does not shorten
// an offer already in flight, even during the last fraction of a millisecond.
func (g OfferGrant) GrantAt(now time.Time) (p.TURNGrant, bool) {
	if !g.validAt(now) {
		return p.TURNGrant{}, false
	}
	grant := g.grant
	if !g.expires.IsZero() {
		// Zero belongs to static credentials, including at this local-only boundary.
		grant.ValidForMS = uint64(max(1, g.expires.Sub(now).Milliseconds()))
	}
	grant.URLs = slices.Clone(grant.URLs)
	return grant, true
}
