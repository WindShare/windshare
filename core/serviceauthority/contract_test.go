package serviceauthority

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
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

func signTestDescriptor(t *testing.T, seedByte byte, domain string, now time.Time, lifetime uint64, mutate func(map[string]any)) ([]byte, [32]byte) {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seedByte}, ed25519.SeedSize))
	pub := priv.Public().(ed25519.PublicKey)
	var key [32]byte
	copy(key[:], pub)
	digest := sha256.Sum256(append([]byte(nodeIdentityDomain), pub...))
	enc := base64.RawURLEncoding.EncodeToString
	issued := uint64(now.Unix())
	doc := map[string]any{
		"version":        2,
		"node_id":        enc(digest[:]),
		"node_key":       enc(pub),
		"relay_endpoint": "wss://node.example/v2/ws",
		"platform":       "native",
		"services":       map[string]bool{"relay": true, "stun": false},
		"stun":           []string{},
		"issued_at":      issued,
		"expires_at":     issued + lifetime,
		"public_relay":   RelayAnonymous,
		"authority":      nil,
	}
	if mutate != nil {
		mutate(doc)
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, append([]byte(domain), payload...))
	raw, err := json.Marshal(map[string]any{
		"version":    2,
		"descriptor": enc(payload),
		"signature":  enc(sig),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw, key
}

func TestInspectGossipDomainSeparationLifetimeAndStructuralInvariants(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	gossipRaw, key := signTestDescriptor(t, 11, GossipDomain, now, GossipLifetime, nil)
	gossip, err := InspectGossip(gossipRaw, now)
	if err != nil || gossip.RelayEndpoint != "wss://node.example/v2/ws" || gossip.PublicRelay != RelayAnonymous {
		t.Fatalf("InspectGossip valid = (%+v, %v)", gossip, err)
	}
	// Gossip-domain envelopes must never pass live descriptor verification and vice versa.
	if _, err := InspectDescriptor(gossipRaw, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("InspectDescriptor accepted gossip envelope: err=%v", err)
	}
	if _, err := VerifyDescriptor(gossipRaw, key, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("VerifyDescriptor accepted gossip envelope: err=%v", err)
	}

	liveRaw, _ := signTestDescriptor(t, 12, DescriptorDomain, now, descriptorLifetime, nil)
	if _, err := InspectDescriptor(liveRaw, now); err != nil {
		t.Fatalf("InspectDescriptor valid = %v", err)
	}
	if _, err := InspectGossip(liveRaw, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("InspectGossip accepted live descriptor envelope: err=%v", err)
	}

	// STUN-only community gossip (relay=false, public_relay=unavailable) is valid.
	stunOnlyRaw, _ := signTestDescriptor(t, 13, GossipDomain, now, GossipLifetime, func(doc map[string]any) {
		doc["services"] = map[string]bool{"relay": false, "stun": true}
		doc["stun"] = []string{"stun:node.example:3478"}
		doc["public_relay"] = RelayUnavailable
	})
	if got, err := InspectGossip(stunOnlyRaw, now); err != nil || !got.Services.STUN || got.Services.Relay {
		t.Fatalf("InspectGossip STUN-only = (%+v, %v)", got, err)
	}

	// Expiry and clock-skew bounds.
	if _, err := InspectGossip(gossipRaw, now.Add(GossipLifetime*time.Second)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired gossip err = %v, want ErrExpired", err)
	}
	futureRaw, _ := signTestDescriptor(t, 14, GossipDomain, now.Add((ClockSkew+1)*time.Second), GossipLifetime, nil)
	if _, err := InspectGossip(futureRaw, now); !errors.Is(err, ErrExpired) {
		t.Fatalf("future-issued gossip err = %v, want ErrExpired", err)
	}

	// Lifetime ceiling and structural invariants.
	overLiveLifetime, _ := signTestDescriptor(t, 15, DescriptorDomain, now, descriptorLifetime+1, nil)
	if _, err := InspectDescriptor(overLiveLifetime, now); !errors.Is(err, ErrFormat) {
		t.Fatalf("live lifetime overflow err = %v, want ErrFormat", err)
	}

	invalidCases := []struct {
		name     string
		lifetime uint64
		mutate   func(map[string]any)
	}{
		{name: "gossip lifetime overflow", lifetime: GossipLifetime + 1},
		{name: "zero lifetime", lifetime: 0},
		{name: "stun service without URLs", lifetime: GossipLifetime, mutate: func(d map[string]any) {
			d["services"] = map[string]bool{"relay": true, "stun": true}
			d["stun"] = []string{}
		}},
		{name: "more than four stun URLs", lifetime: GossipLifetime, mutate: func(d map[string]any) {
			d["services"] = map[string]bool{"relay": true, "stun": true}
			d["stun"] = []string{"stun:a.example:3478", "stun:b.example:3478", "stun:c.example:3478", "stun:d.example:3478", "stun:e.example:3478"}
		}},
		{name: "anonymous public relay without relay service", lifetime: GossipLifetime, mutate: func(d map[string]any) {
			d["services"] = map[string]bool{"relay": false, "stun": false}
			d["public_relay"] = RelayAnonymous
		}},
		{name: "authenticated public relay without relay service", lifetime: GossipLifetime, mutate: func(d map[string]any) {
			d["services"] = map[string]bool{"relay": false, "stun": false}
			d["public_relay"] = RelayAuthenticated
		}},
		{name: "invalid authority issuer", lifetime: GossipLifetime, mutate: func(d map[string]any) {
			d["authority"] = map[string]any{"url": "https://api.example", "issuer": "not-a-key"}
		}},
		{name: "mismatched node_id", lifetime: GossipLifetime, mutate: func(d map[string]any) {
			d["node_id"] = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		}},
	}
	for idx, tc := range invalidCases {
		raw, _ := signTestDescriptor(t, byte(20+idx), GossipDomain, now, tc.lifetime, tc.mutate)
		if _, err := InspectGossip(raw, now); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: err = %v, want ErrFormat", tc.name, err)
		}
	}

	if _, err := InspectGossip(bytes.Repeat([]byte("a"), MaximumDescriptorBytes+1), now); !errors.Is(err, ErrFormat) {
		t.Fatalf("oversized envelope err = %v, want ErrFormat", err)
	}
}

func TestDescriptorOffersCommunityService(t *testing.T) {
	auth := &struct {
		URL    string `json:"url"`
		Issuer string `json:"issuer"`
	}{URL: "https://api.example", Issuer: "issuer"}
	cases := []struct {
		name  string
		relay bool
		stun  bool
		state PublicRelay
		auth  bool
		want  bool
	}{
		{name: "anonymous relay only", relay: true, state: RelayAnonymous, want: true},
		{name: "dual service", relay: true, stun: true, state: RelayAnonymous, want: true},
		{name: "stun only with unavailable relay", stun: true, state: RelayUnavailable, want: true},
		{name: "stun with budget exhausted relay", relay: true, stun: true, state: RelayBudgetExhausted, want: true},
		{name: "stun with authenticated relay", relay: true, stun: true, state: RelayAuthenticated, want: true},
		{name: "opted out disables stun too", relay: true, stun: true, state: RelayDisabled, want: false},
		{name: "authority managed never joins community", relay: true, stun: true, state: RelayAnonymous, auth: true, want: false},
		{name: "authenticated relay without stun", relay: true, state: RelayAuthenticated, want: false},
		{name: "budget exhausted relay without stun", relay: true, state: RelayBudgetExhausted, want: false},
		{name: "unavailable relay without stun", relay: true, state: RelayUnavailable, want: false},
		{name: "anonymous state without relay service", relay: false, stun: false, state: RelayAnonymous, want: false},
	}
	for _, tc := range cases {
		var d Descriptor
		d.Services.Relay, d.Services.STUN = tc.relay, tc.stun
		d.PublicRelay = tc.state
		if tc.auth {
			d.Authority = auth
		}
		if got := d.OffersCommunityService(); got != tc.want {
			t.Errorf("%s: OffersCommunityService() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
