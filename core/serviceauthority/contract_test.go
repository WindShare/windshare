package serviceauthority

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestRustAuthorityContractsAndMonotonicIdentity(t *testing.T) {
	data, err := os.ReadFile("../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Now             int64
		Root            string
		Session         struct{ Ticket, Certificate string }
		Refreshed       struct{ Ticket, Certificate string }
		NextCertificate string `json:"next_certificate"`
		Noncanonical    string `json:"noncanonical_ticket"`
		NodeKey         string `json:"node_key"`
		Descriptor      json.RawMessage
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	root, err := PublicKey(fixture.Root)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := VerifyGeneration(fixture.Session.Certificate, root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(fixture.Now, 0)
	ticket, err := VerifyTicket(fixture.Session.Ticket, generation, now)
	if err != nil {
		t.Fatal(err)
	}
	if !(Entitlement{}).ResetAt().IsZero() {
		t.Fatalf("zero entitlement reported reset time %v", (Entitlement{}).ResetAt())
	}
	if !ticket.Entitlement.ResetAt().Equal(time.Unix(int64((ticket.Entitlement.Day+1)*DaySeconds), 0).UTC()) {
		t.Fatalf("issued entitlement reset time = %v", ticket.Entitlement.ResetAt())
	}
	refreshed, err := VerifyTicket(fixture.Refreshed.Ticket, generation, now)
	if err != nil || refreshed.Account != ticket.Account || refreshed.Credential != ticket.Credential ||
		refreshed.Entitlement != ticket.Entitlement {
		t.Fatal(refreshed, err)
	}
	if _, err := VerifyTicket(fixture.Noncanonical, generation, now); !errors.Is(err, ErrFormat) {
		t.Fatal("noncanonical CBOR accepted", err)
	}
	if _, err := VerifyTicket(fixture.Session.Ticket, generation, time.Unix(int64(ticket.ExpiresAt), 0)); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}
	stale, err := VerifyTicket(fixture.Session.Ticket, generation, now.Add(6*time.Minute))
	if err != nil || stale.Entitlement.Allowance(now.Add(6*time.Minute)) != AllowanceStale {
		t.Fatal(stale, err)
	}
	renewed := ticket.Entitlement
	renewed.ValidUntil += 60
	if Advance(ticket.Entitlement, renewed) != renewed || Advance(renewed, ticket.Entitlement) != renewed {
		t.Fatal("lease renewal did not advance monotonically")
	}
	spent := refreshed.Entitlement
	spent.Version++
	spent.RemainingBytes = 0
	if Advance(renewed, spent) != spent || Advance(spent, renewed) != spent ||
		Advance(spent, ticket.Entitlement) != spent || spent.Allowance(now) != AllowanceSpent {
		t.Fatal("old ticket replenished allowance")
	}
	nextDay := Entitlement{
		Version:        1,
		Day:            spent.Day + 1,
		ValidUntil:     (spent.Day+1)*DaySeconds + EntitlementLifetime,
		RemainingBytes: ticket.Entitlement.RemainingBytes,
	}
	if Advance(spent, nextDay) != nextDay || Advance(nextDay, spent) != nextDay {
		t.Fatal("new UTC day did not supersede prior day version")
	}
	newer, err := VerifyGeneration(fixture.NextCertificate, root)
	if err != nil || Compare(generation, newer) != nil || !errors.Is(Compare(newer, generation), ErrRollback) {
		t.Fatal(newer, err)
	}
	if _, err := VerifyTicket(fixture.Session.Ticket, newer, now); !errors.Is(err, ErrRollback) {
		t.Fatal("old signing generation accepted", err)
	}
	key, _ := PublicKey(fixture.NodeKey)
	descriptor, err := VerifyDescriptor(fixture.Descriptor, key, now)
	if err != nil || descriptor.PublicRelay != RelayAnonymous || !descriptor.Services.Relay {
		t.Fatal(descriptor, err)
	}
	if _, err := VerifyDescriptor(fixture.Descriptor, root, now); !errors.Is(err, ErrSignature) {
		t.Fatal("unbound node accepted", err)
	}
}

func TestPublicRelayRequiresCompleteTaggedState(t *testing.T) {
	for _, wire := range []string{`{"state":"enabled"}`, `{"state":"enabled","anonymous":null}`, `{"state":"disabled","anonymous":false}`, `{"state":"unavailable","anonymous":null}`, `{"state":"disabled","enabled":true}`} {
		var state PublicRelay
		if err := json.Unmarshal([]byte(wire), &state); err == nil {
			t.Errorf("accepted %s", wire)
		}
	}
	for _, want := range []PublicRelay{RelayAnonymous, RelayAuthenticated, RelayBudgetExhausted, RelayUnavailable, RelayDisabled} {
		bytes, err := json.Marshal(want)
		var got PublicRelay
		if err != nil || json.Unmarshal(bytes, &got) != nil || got != want {
			t.Fatal(want, got, err)
		}
	}
}
