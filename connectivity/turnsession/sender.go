package turnsession

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/session/turngrant"

	diag "github.com/windshare/windshare/core/diagnostics"
	p "github.com/windshare/windshare/core/session/protocolsession"
)

const refreshInterval = 2 * time.Second
const maxPending = 8

type entry struct {
	grant   p.TURNGrant
	expires time.Time
}
type Sender struct {
	mu       sync.Mutex
	provider Provider
	now      func() time.Time
	trace    turngrant.Trace
	issued   map[p.TURNGrantID]entry
	latest   map[string]p.TURNGrantID
	previous map[string]p.TURNGrantID
	refresh  map[string]time.Time
	full     map[p.TURNMode][]uint64
	active   int
	closed   bool
	work     sync.WaitGroup
}

func NewSender(trace turngrant.Trace, provider Provider, now func() time.Time) *Sender {
	return &Sender{trace: trace, provider: provider, now: now, issued: make(map[p.TURNGrantID]entry), latest: make(map[string]p.TURNGrantID), previous: make(map[string]p.TURNGrantID), refresh: make(map[string]time.Time), full: make(map[p.TURNMode][]uint64)}
}
func (s *Sender) Close() { s.mu.Lock(); s.closed = true; s.mu.Unlock(); s.work.Wait() }

func (s *Sender) Lookup(id p.TURNGrantID) (p.TURNGrant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookup(id)
}
func (s *Sender) lookup(id p.TURNGrantID) (p.TURNGrant, bool) {
	e, ok := s.issued[id]
	if !ok {
		return p.TURNGrant{}, false
	}
	if !e.expires.IsZero() {
		left := e.expires.Sub(s.now()).Milliseconds()
		if left <= 0 {
			delete(s.issued, id)
			return p.TURNGrant{}, false
		}
		e.grant.ValidForMS = uint64(left)
	}
	return e.grant, true
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
			reply.RequestID = id
			data, err := p.EncodeTURNGrants(reply)
			if err != nil {
				data, _ = p.EncodeTURNGrants(p.TURNGrants{RequestID: id, Status: p.TURNRetryLater, HasFallback: reply.HasFallback})
			}
			if send(ctx, data) != nil {
				return
			}
		}
	}()
	return nil
}

func (s *Sender) resolve(ctx context.Context, d p.TURNDemand) p.TURNGrants {
	reply := p.TURNGrants{RequestID: d.RequestID, Status: p.TURNUnconfigured}
	if s.provider == nil {
		return reply
	}
	request := turngrant.IssueRequest{Trace: s.trace, Mode: d.Mode, ServiceIDs: d.ServiceIDs}
	s.mu.Lock()
	if len(d.ServiceIDs) == 0 {
		request.Reusable = make(map[string]bool, len(s.latest))
		for service, id := range s.latest {
			if _, ok := s.lookup(id); ok {
				request.Reusable[service] = true
			}
		}
	} else {
		for _, id := range d.ServiceIDs {
			// A targeted refresh can spend only an already issued service's budget.
			_, known := s.refresh[id]
			if !known || s.now().Before(s.refresh[id]) {
				s.mu.Unlock()
				reply.Status = p.TURNRetryLater
				return reply
			}
		}
		for _, id := range d.ServiceIDs {
			s.refresh[id] = s.now().Add(refreshInterval)
		}
	}
	s.mu.Unlock()
	result := s.provider.Issue(ctx, request)
	reply.Status, reply.HasFallback = result.Status, result.HasFallback
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, service := range result.Retained {
		if grant, ok := s.lookup(s.latest[service]); ok {
			reply.Grants = append(reply.Grants, grant)
		}
	}
	for _, c := range result.Credentials {
		grant := p.TURNGrant{ServiceID: c.ServiceID, URLs: c.URLs, Username: c.Username, Password: c.Password, ValidForMS: uint64(c.ValidFor.Milliseconds())}
		rand.Read(grant.GrantID[:])
		if !grant.Valid() {
			continue
		}
		// Keep one predecessor while an authenticated replacement is negotiating.
		delete(s.issued, s.previous[c.ServiceID])
		s.previous[c.ServiceID], s.latest[c.ServiceID] = s.latest[c.ServiceID], grant.GrantID
		var expiry time.Time
		if c.ValidFor > 0 {
			expiry = s.now().Add(c.ValidFor)
		}
		s.issued[grant.GrantID] = entry{grant, expiry}
		s.refresh[c.ServiceID] = s.now().Add(refreshInterval)
		reply.Grants = append(reply.Grants, grant)
		s.trace.Event("turn_grant_issued", c.ServiceID, diag.Fields{"grant_id": diag.Identity(grant.GrantID[:]), "ttl_ms": diag.Uint(grant.ValidForMS), "url_count": diag.Uint(uint64(len(grant.URLs)))})
	}
	if len(reply.Grants) == 0 && len(result.Retained)+len(result.Credentials) > 0 {
		reply.Status = p.TURNRetryLater
	}
	return reply
}

var ErrUnknownGrant = errors.New("TURN grant is missing or expired")
