package nodedirectory

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type fixture struct {
	root, online ed25519.PrivateKey
	roots        []Root
	now          time.Time
}

func newFixture() fixture {
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	online := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	return fixture{root: root, online: online, roots: []Root{{ID: "primary", Key: root.Public().(ed25519.PublicKey)}}, now: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)}
}
func (f fixture) payload(generation, serial uint64) Payload {
	return Payload{Format: Format, Network: "test", Generation: generation, Serial: serial, IssuedAt: f.now.Add(-time.Hour), ExpiresAt: f.now.Add(24 * time.Hour),
		Nodes: []Node{{Alias: "jp1", Relay: "https://jp1.example", Roles: []string{"relay"}, Zone: "global", Region: "jp", Provider: "provider", Operator: "operator", CapacityMbps: 50, Status: Active}}}
}
func (f fixture) certificate(generation uint64) SignedCertificate {
	certificate, _ := Certify(Certificate{Format: Format, Network: "test", Generation: generation, Key: encoding.EncodeToString(f.online.Public().(ed25519.PublicKey)),
		NotBefore: f.now.Add(-24 * time.Hour), ExpiresAt: f.now.Add(30 * 24 * time.Hour)}, "primary", f.root)
	return certificate
}
func (f fixture) signed(t *testing.T, payload Payload) []byte {
	t.Helper()
	encoded, err := Sign(payload, f.certificate(payload.Generation), f.online, f.roots)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
func (f fixture) snapshot(t *testing.T, payload Payload) Snapshot {
	t.Helper()
	snapshot, err := Verify(f.signed(t, payload), "test", f.roots)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestGenerationSerialAndEquivocation(t *testing.T) {
	f := newFixture()
	first := f.snapshot(t, f.payload(1, 42))
	for _, test := range []struct {
		generation, serial uint64
		want               error
	}{{1, 41, ErrRollback}, {1, 42, nil}, {1, 43, nil}, {2, 1, nil}} {
		candidate := f.snapshot(t, f.payload(test.generation, test.serial))
		if err := Accept(first.Cursor(), candidate, f.now, DefaultGrace); !errors.Is(err, test.want) {
			t.Fatalf("%+v: %v", test, err)
		}
	}
	changed := f.payload(1, 42)
	changed.Nodes[0].CapacityMbps++
	if err := Accept(first.Cursor(), f.snapshot(t, changed), f.now, DefaultGrace); !errors.Is(err, ErrRollback) {
		t.Fatal(err)
	}
	if _, err := Sign(f.payload(2, 1), f.certificate(1), f.online, f.roots); !errors.Is(err, ErrFormat) {
		t.Fatal("online key advanced generation", err)
	}
}

func TestChainRejectsTamperingAndCrossNetworkUse(t *testing.T) {
	f := newFixture()
	data := f.signed(t, f.payload(1, 1))
	for _, mutate := range []func(*Envelope){
		func(e *Envelope) { e.Certificate.Root = "unknown" },
		func(e *Envelope) { e.Signature = encoding.EncodeToString(make([]byte, ed25519.SignatureSize)) },
		func(e *Envelope) { e.Payload = encoding.EncodeToString([]byte(`{"format":1}`)) },
		func(e *Envelope) { e.Certificate.Signature = e.Signature },
	} {
		var envelope Envelope
		_ = json.Unmarshal(data, &envelope)
		mutate(&envelope)
		encoded, _ := json.Marshal(envelope)
		if _, err := Verify(encoded, "test", f.roots); err == nil {
			t.Fatal("accepted modified chain")
		}
	}
	if _, err := Verify(data, "other", f.roots); err == nil {
		t.Fatal("cross-network certificate accepted")
	}
}

func TestTrustExpiryAndSnapshotIsolation(t *testing.T) {
	f := newFixture()
	snapshot := f.snapshot(t, f.payload(1, 1))
	if !snapshot.Usable(f.now.Add(7*24*time.Hour), DefaultGrace) || snapshot.Usable(f.now.Add(8*24*time.Hour), DefaultGrace) {
		t.Fatal("grace not bounded")
	}
	payload := snapshot.Payload()
	payload.Nodes[0].Relay = "https://evil.example"
	payload.Nodes[0].Roles[0] = "stun"
	if endpoint, _ := snapshot.Resolve("jp1"); endpoint != "https://jp1.example" {
		t.Fatal(endpoint)
	}
	data := snapshot.Envelope()
	data[0] = 0
	if _, err := Verify(snapshot.Envelope(), "test", f.roots); err != nil {
		t.Fatal(err)
	}
	payload = f.payload(1, 2)
	payload.ExpiresAt = f.now.Add(60 * 24 * time.Hour)
	long := f.snapshot(t, payload)
	if long.Usable(f.now.Add(30*24*time.Hour), DefaultGrace) {
		t.Fatal("grace extended certificate")
	}
}

func TestAliasTombstonesSurviveKeyRotation(t *testing.T) {
	f := newFixture()
	first := f.snapshot(t, f.payload(1, 1))
	aliases, err := RetainAliases(nil, first)
	if err != nil {
		t.Fatal(err)
	}
	removed := f.payload(2, 1)
	removed.Nodes = nil
	aliases, err = RetainAliases(aliases, f.snapshot(t, removed))
	if err != nil {
		t.Fatal(err)
	}
	replacement := f.payload(2, 2)
	replacement.Nodes[0].Relay = "https://new.example"
	if _, err = RetainAliases(aliases, f.snapshot(t, replacement)); !errors.Is(err, ErrAliasReassigned) {
		t.Fatal(err)
	}
}

func TestStrictPayloadAndEnvelopeBounds(t *testing.T) {
	f := newFixture()
	for _, mutate := range []func(*Payload){
		func(p *Payload) { p.Nodes = append(p.Nodes, p.Nodes[0]) },
		func(p *Payload) {
			duplicate := p.Nodes[0]
			duplicate.Alias = "jp2"
			p.Nodes = append(p.Nodes, duplicate)
		},
		func(p *Payload) { p.Nodes[0].Relay = "https://jp1.example?key=secret" },
		func(p *Payload) { p.Nodes[0].Roles = []string{"unknown"} },
		func(p *Payload) { p.Serial = MaximumCounter + 1 },
		func(p *Payload) { p.ExpiresAt = p.IssuedAt.Add(MaximumPayloadLifetime + time.Second) },
		func(p *Payload) { p.Nodes[0].Alias = "../jp1" },
	} {
		payload := f.payload(1, 1)
		mutate(&payload)
		if err := Validate(payload); err == nil {
			t.Fatal("accepted invalid payload")
		}
	}
	if _, err := Verify(make([]byte, MaximumEnvelopeBytes+1), "test", f.roots); err == nil {
		t.Fatal("unbounded envelope")
	}
	if err := decode([]byte(`{"serial":1,"serial":2}`), new(Payload)); err == nil {
		t.Fatal("duplicate field")
	}
}
