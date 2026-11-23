package turnsession

import (
	"context"
	"crypto/rand"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/windshare/windshare/core/session/turngrant"

	diag "github.com/windshare/windshare/core/diagnostics"
	p "github.com/windshare/windshare/core/session/protocolsession"
)

const refreshInterval = 2 * time.Second
const maxPending = 8

type Sender struct {
	mu       sync.Mutex
	provider Provider
	now      func() time.Time
	trace    turngrant.Trace
	issued   map[p.TURNGrantID]OfferGrant
	latest   map[string]p.TURNGrantID
	previous map[string]p.TURNGrantID
	refresh  map[string]time.Time
	full     map[p.TURNMode][]uint64
	active   int
	closed   bool
	work     sync.WaitGroup
}

func NewSender(trace turngrant.Trace, provider Provider, now func() time.Time) *Sender {
	return &Sender{trace: trace, provider: provider, now: now, issued: make(map[p.TURNGrantID]OfferGrant), latest: make(map[string]p.TURNGrantID), previous: make(map[string]p.TURNGrantID), refresh: make(map[string]time.Time), full: make(map[p.TURNMode][]uint64)}
}
func (s *Sender) Close() { s.mu.Lock(); s.closed = true; s.mu.Unlock(); s.work.Wait() }

// Lookup returns the remaining lifetime representable on the wire.
func (s *Sender) Lookup(id p.TURNGrantID) (p.TURNGrant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.lookup(id)
	grant.URLs = slices.Clone(grant.URLs)
	return grant, ok
}
func (s *Sender) lookup(id p.TURNGrantID) (p.TURNGrant, bool) {
	e, ok := s.issued[id]
	if !ok {
		return p.TURNGrant{}, false
	}
	if !e.expires.IsZero() {
		remaining := e.expires.Sub(s.now())
		if remaining <= 0 {
			delete(s.issued, id)
			return p.TURNGrant{}, false
		}
		if remaining < time.Millisecond {
			return p.TURNGrant{}, false
		}
		e.grant.ValidForMS = uint64(remaining.Milliseconds())
	}
	return e.grant, true
}

// Full replies need enough remaining lifetime to start another negotiation.
// Offers already carrying this ID keep their authorization until actual expiry.
func (s *Sender) reusable(id p.TURNGrantID) (p.TURNGrant, bool) {
	grant, ok := s.lookup(id)
	if !ok || grant.ValidForMS == 0 {
		return grant, ok
	}
	lifetime := time.Duration(s.issued[id].grant.ValidForMS) * time.Millisecond
	remaining := time.Duration(grant.ValidForMS) * time.Millisecond
	return grant, remaining > turngrant.RenewalLead(lifetime)
}

type Send func(context.Context, []byte) error

func (s *Sender) Handle(ctx context.Context, body []byte, send Send) error {
	d, err := p.DecodeTURNDemand(body)
	if err != nil {
		return err
	}
	s.trace.Event("turn_demand_received", "", diag.Fields{"mode": diag.Uint(uint64(d.Mode)), "request_id": diag.Uint(d.RequestID)})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	full := len(d.ServiceIDs) == 0
	if waiting := s.full[d.Mode]; full && len(waiting) > 0 {
		if len(waiting) < maxPending {
			s.full[d.Mode] = append(waiting, d.RequestID)
		}
		s.mu.Unlock()
		return nil
	}
	if s.active >= maxPending {
		s.mu.Unlock()
		return nil
	}
	if full {
		s.full[d.Mode] = []uint64{d.RequestID}
	}
	s.active++
	s.work.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.work.Done()
		reply := s.resolve(ctx, d)
		ids := []uint64{d.RequestID}
		s.mu.Lock()
		if full {
			ids = s.full[d.Mode]
			delete(s.full, d.Mode)
		}
		s.mu.Unlock()
		defer func() { s.mu.Lock(); s.active--; s.mu.Unlock() }()
		for _, id := range ids {
			if send(ctx, s.encodeReply(id, reply)) != nil {
				return
			}
		}
	}()
	return nil
}

// A coalesced caller can wait behind another write. Compute its wire lifetime
// when it is sent, while the cache can still authorize a new offer for this ID.
func (s *Sender) encodeReply(id uint64, issued p.TURNGrants) []byte {
	reply := issued
	reply.RequestID = id
	if len(issued.Grants) != 0 {
		reply.Grants = make([]p.TURNGrant, 0, len(issued.Grants))
		s.mu.Lock()
		for _, g := range issued.Grants {
			if grant, ok := s.reusable(g.GrantID); ok {
				reply.Grants = append(reply.Grants, grant)
			}
		}
		s.mu.Unlock()
		if len(reply.Grants) != len(issued.Grants) {
			reply.Status = p.TURNRetryStatus(len(reply.Grants))
		}
	}
	data, err := p.EncodeTURNGrants(reply)
	if err != nil {
		data, _ = p.EncodeTURNGrants(p.TURNGrants{RequestID: id, Status: p.TURNRetryLater, HasFallback: reply.HasFallback})
	}
	return data
}

func (s *Sender) resolve(ctx context.Context, d p.TURNDemand) p.TURNGrants {
	reply := p.TURNGrants{RequestID: d.RequestID, Status: p.TURNUnconfigured}
	if s.provider == nil {
		return reply
	}
	request, prior, admitted := s.prepareIssue(d)
	if !admitted {
		reply.Status = p.TURNRetryLater
		return reply
	}
	result := s.provider.Issue(ctx, request)
	reply.Status, reply.HasFallback = result.Status, result.HasFallback
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, service := range result.Retained {
		if grant, ok := s.reusable(s.latest[service]); ok {
			reply.Grants = append(reply.Grants, grant)
		}
	}
	for _, credential := range result.Credentials {
		if grant, ok := s.install(credential, prior[credential.ServiceID]); ok {
			reply.Grants = append(reply.Grants, grant)
		}
	}
	if len(reply.Grants) < len(result.Retained)+len(result.Credentials) {
		reply.Status = p.TURNRetryStatus(len(reply.Grants))
	}
	return reply
}

func (s *Sender) prepareIssue(d p.TURNDemand) (turngrant.IssueRequest, map[string]p.TURNGrantID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	request := turngrant.IssueRequest{Trace: s.trace, Mode: d.Mode, ServiceIDs: d.ServiceIDs}
	if len(d.ServiceIDs) == 0 {
		request.Reusable = make(map[string]bool, len(s.latest))
		for service, id := range s.latest {
			if _, ok := s.reusable(id); ok {
				request.Reusable[service] = true
			}
		}
	} else {
		now := s.now()
		for _, id := range d.ServiceIDs {
			// A targeted refresh can spend only an already issued service's budget.
			due, known := s.refresh[id]
			if !known || now.Before(due) {
				return request, nil, false
			}
		}
		for _, id := range d.ServiceIDs {
			s.refresh[id] = now.Add(refreshInterval)
		}
	}
	return request, maps.Clone(s.latest), true
}

func (s *Sender) install(c turngrant.Credential, prior p.TURNGrantID) (p.TURNGrant, bool) {
	now := s.now()
	var validForMS uint64
	if !c.ExpiresAt.IsZero() {
		// Only a static credential can advertise zero lifetime on the wire.
		remaining := c.ExpiresAt.Sub(now).Milliseconds()
		if remaining <= 0 {
			return p.TURNGrant{}, false
		}
		validForMS = uint64(remaining)
	}
	// A targeted renewal and full escalation may finish together. Keep the
	// winning rotation so both replies leave the original offer authorized.
	if current := s.latest[c.ServiceID]; current != prior {
		if grant, ok := s.reusable(current); ok {
			return grant, true
		}
	}
	grant := p.TURNGrant{ServiceID: c.ServiceID, URLs: slices.Clone(c.URLs), Username: c.Username, Password: c.Password, ValidForMS: validForMS}
	rand.Read(grant.GrantID[:])
	if !grant.Valid() {
		return p.TURNGrant{}, false
	}
	// Keep one predecessor while an authenticated replacement is negotiating.
	delete(s.issued, s.previous[c.ServiceID])
	s.previous[c.ServiceID], s.latest[c.ServiceID] = s.latest[c.ServiceID], grant.GrantID
	s.issued[grant.GrantID] = OfferGrant{grant: grant, expires: c.ExpiresAt}
	s.refresh[c.ServiceID] = now.Add(refreshInterval)
	s.trace.Event("turn_grant_issued", c.ServiceID, diag.Fields{"grant_id": diag.Identity(grant.GrantID[:]), "ttl_ms": diag.Uint(grant.ValidForMS), "url_count": diag.Uint(uint64(len(grant.URLs)))})
	return grant, true
}

var ErrUnknownGrant = errors.New("TURN grant is missing or expired")
