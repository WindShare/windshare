package turnsession

import (
	"context"
	"testing"
	"time"

	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/turngrant"
)

func testCredential(id string, expiresAt time.Time) turngrant.Credential {
	return turngrant.Credential{ServiceID: id, URLs: []string{"turn:local"}, Username: "u", Password: "p", ExpiresAt: expiresAt}
}

func TestFullGrantFreshnessPreservesOutstandingOffer(t *testing.T) {
	now := time.Unix(100, 0)
	issued := 0
	provider := issueFunc(func(_ context.Context, request turngrant.IssueRequest) turngrant.IssueResult {
		if request.Reusable["home"] {
			return turngrant.IssueResult{Status: p.TURNOK, Retained: []string{"home"}}
		}
		issued++
		return turngrant.IssueResult{Status: p.TURNOK, Credentials: []turngrant.Credential{testCredential("home", now.Add(time.Minute))}}
	})
	sender := NewSender(turngrant.Trace{}, provider, func() time.Time { return now })
	defer sender.Close()
	demand := p.TURNDemand{RequestID: 1, Mode: p.TURNAll}
	original := sender.resolve(t.Context(), demand).Grants[0]
	now = now.Add(44 * time.Second)
	retained := sender.resolve(t.Context(), demand)
	if issued != 1 || retained.Grants[0].GrantID != original.GrantID || retained.Grants[0].ValidForMS != 16000 {
		t.Fatal("still-fresh grant was reissued or its original expiry moved")
	}
	now = now.Add(time.Second)
	fresh := sender.resolve(t.Context(), demand)
	if issued != 2 || fresh.Grants[0].GrantID == original.GrantID {
		t.Fatal("near-expiry grant advertised instead of renewing")
	}
	if old, ok := sender.Lookup(original.GrantID); !ok || old.ValidForMS != 15000 {
		t.Fatal("outstanding offer lost its original authorization")
	}
	now = now.Add(15 * time.Second)
	if _, ok := sender.Lookup(original.GrantID); ok {
		t.Fatal("expired authorization accepted")
	}
}

func TestConcurrentFullAndTargetedRenewalKeepTheOutstandingOffer(t *testing.T) {
	now := time.Unix(100, 0)
	targetStarted, finishTarget := make(chan struct{}), make(chan struct{})
	provider := issueFunc(func(_ context.Context, request turngrant.IssueRequest) turngrant.IssueResult {
		if len(request.ServiceIDs) != 0 {
			close(targetStarted)
			<-finishTarget
		}
		return turngrant.IssueResult{Status: p.TURNOK, Credentials: []turngrant.Credential{testCredential("home", now.Add(time.Minute))}}
	})
	sender := NewSender(turngrant.Trace{}, provider, func() time.Time { return now })
	defer sender.Close()
	full := p.TURNDemand{RequestID: 1, Mode: p.TURNAll}
	original := sender.resolve(t.Context(), full).Grants[0]
	now = now.Add(45 * time.Second)
	renewed := make(chan p.TURNGrants, 1)
	go func() {
		renewed <- sender.resolve(t.Context(), p.TURNDemand{RequestID: 2, Mode: p.TURNAll, ServiceIDs: []string{"home"}})
	}()
	<-targetStarted
	escalated := sender.resolve(t.Context(), full).Grants[0]
	close(finishTarget)
	refresh := <-renewed
	if len(refresh.Grants) != 1 || refresh.Grants[0].GrantID != escalated.GrantID || escalated.GrantID == original.GrantID {
		t.Fatal("concurrent renewal rotated the same service twice")
	}
	if _, ok := sender.Lookup(original.GrantID); !ok {
		t.Fatal("concurrent renewal invalidated a live outstanding offer")
	}
}

func TestGrantExpiryDuringIssuancePreservesUsableReplies(t *testing.T) {
	for _, tc := range []struct {
		name string
		more []turngrant.Credential
		want p.TURNStatus
	}{
		{"only source", nil, p.TURNRetryLater},
		{"another succeeds", []turngrant.Credential{testCredential("other", time.Time{})}, p.TURNPartialRetryable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(100, 0)
			provider := issueFunc(func(_ context.Context, r turngrant.IssueRequest) turngrant.IssueResult {
				if !r.Reusable["home"] {
					return turngrant.IssueResult{Status: p.TURNOK, Credentials: []turngrant.Credential{testCredential("home", now.Add(time.Minute))}}
				}
				now = now.Add(time.Minute)
				return turngrant.IssueResult{Status: p.TURNOK, Retained: []string{"home"}, Credentials: tc.more}
			})
			sender := NewSender(turngrant.Trace{}, provider, func() time.Time { return now })
			defer sender.Close()
			demand := p.TURNDemand{RequestID: 1, Mode: p.TURNAll}
			_ = sender.resolve(t.Context(), demand)
			got := sender.resolve(t.Context(), demand)
			if got.Status != tc.want || len(got.Grants) != len(tc.more) {
				t.Fatal("lost the still-usable grant or the retry signal")
			}
			if len(got.Grants) != 0 && got.Grants[0].ServiceID != "other" {
				t.Fatal("expired grant advertised")
			}
		})
	}
}

func TestSenderLifetimeConversionKeepsOnlyExplicitStaticCredentials(t *testing.T) {
	now := time.Unix(100, 0)
	provider := issueFunc(func(context.Context, turngrant.IssueRequest) turngrant.IssueResult {
		return turngrant.IssueResult{Status: p.TURNOK, Credentials: []turngrant.Credential{
			testCredential("expired", now.Add(-time.Second)), testCredential("sub-millisecond", now.Add(time.Nanosecond)), testCredential("static", time.Time{}),
		}}
	})
	sender := NewSender(turngrant.Trace{}, provider, func() time.Time { return now })
	defer sender.Close()
	got := sender.resolve(t.Context(), p.TURNDemand{RequestID: 1, Mode: p.TURNAll})
	if got.Status != p.TURNPartialRetryable || len(got.Grants) != 1 || got.Grants[0].ServiceID != "static" || got.Grants[0].ValidForMS != 0 || len(sender.issued) != 1 {
		t.Fatal("unrepresentable lifetime reached the wire or cache")
	}
	now = now.Add(48 * time.Hour)
	if _, ok := sender.Lookup(got.Grants[0].GrantID); !ok {
		t.Fatal("explicit static credential expired")
	}
}
