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
	refreshed, err := VerifyTicket(fixture.Refreshed.Ticket, generation, now)
	if err != nil || refreshed.Account != ticket.Account || refreshed.Credential != ticket.Credential ||
		refreshed.Entitlement.Version <= ticket.Entitlement.Version {
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
	spent := refreshed.Entitlement
	spent.Version++
	spent.RemainingBytes = 0
	if Advance(spent, ticket.Entitlement) != spent || spent.Allowance(now) != AllowanceSpent {
		t.Fatal("old ticket replenished allowance")
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
	if err != nil || !descriptor.Anonymous || !descriptor.Services.Relay {
		t.Fatal(descriptor, err)
	}
	if _, err := VerifyDescriptor(fixture.Descriptor, root, now); !errors.Is(err, ErrSignature) {
		t.Fatal("unbound node accepted", err)
	}
}
