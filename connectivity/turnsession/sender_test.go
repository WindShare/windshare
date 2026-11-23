package turnsession

import (
	"context"
	"testing"
	"time"

	p "github.com/windshare/windshare/core/session/protocolsession"
)

type issueFunc func(context.Context, IssueRequest) IssueResult

func (f issueFunc) Issue(ctx context.Context, r IssueRequest) IssueResult { return f(ctx, r) }

func TestSessionCoalescesDemandAndRenewsOneGrantAtATime(t *testing.T) {
	clock := time.Unix(200, 0)
	calls := 0
	entered, release := make(chan struct{}), make(chan struct{})
	provider := issueFunc(func(_ context.Context, r IssueRequest) IssueResult {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		if r.Reusable["a"] {
			return IssueResult{Status: p.TURNOK, Retained: []string{"a"}}
		}
		return IssueResult{Status: p.TURNOK, Credentials: []Credential{{ServiceID: "a", URLs: []string{"turn:one"}, Username: "u", Password: "p", ValidFor: time.Minute}}}
	})
	s := NewSender(p.ProtocolSessionID{1}, provider, func() time.Time { return clock })
	defer s.Close()
	responses := make(chan p.TURNGrants, 2)
	send := func(_ context.Context, b []byte) error { g, err := p.DecodeTURNGrants(b); responses <- g; return err }
	for n := uint64(1); n <= 2; n++ {
		body, _ := p.EncodeTURNDemand(p.TURNDemand{RequestID: n, Mode: p.TURNParallel})
		if err := s.Handle(t.Context(), body, send); err != nil {
			t.Fatal(err)
		}
		<-entered
	}
	close(release)
	first, second := <-responses, <-responses
	if calls != 1 || first.RequestID == second.RequestID || first.Grants[0].GrantID != second.Grants[0].GrantID {
		t.Fatal("identical demand did not share its grant")
	}
	id := first.Grants[0].GrantID
	more := s.resolve(t.Context(), p.TURNDemand{RequestID: 3, Mode: p.TURNAll})
	if more.Grants[0].GrantID != id {
		t.Fatal("escalation replaced a live grant")
	}
	refresh := p.TURNDemand{RequestID: 4, Mode: p.TURNAll, ServiceIDs: []string{"a"}}
	if s.resolve(t.Context(), refresh).Status != p.TURNRetryLater {
		t.Fatal("refresh bypassed per-service throttle")
	}
	clock = clock.Add(refreshInterval)
	updated := s.resolve(t.Context(), refresh).Grants[0]
	if updated.GrantID == id {
		t.Fatal("refresh did not rotate")
	}
	if _, ok := s.Lookup(id); !ok {
		t.Fatal("renewal prematurely invalidated an in-flight offer")
	}
	clock = clock.Add(time.Minute)
	if _, ok := s.Lookup(updated.GrantID); ok {
		t.Fatal("expired authorization accepted")
	}
}
