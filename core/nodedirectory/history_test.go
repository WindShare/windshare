package nodedirectory

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
)

func TestHistoryPreservesPreIdentityAliasesWithoutGrantingRuntimeTrust(t *testing.T) {
	fixture := newFixture()
	payload := fixture.payload(1, 2)
	payload.Nodes[0].NodeKey = ""
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope := Envelope{Certificate: fixture.certificate(1), Payload: encoding.EncodeToString(body),
		Signature: encoding.EncodeToString(ed25519.Sign(fixture.online, append([]byte(PayloadDomain), body...)))}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	history, err := VerifyHistory(data, "test", fixture.roots)
	if err != nil {
		t.Fatal(err)
	}
	if history.Cursor().Serial != 2 {
		t.Fatal("historical rollback barrier changed")
	}
	if _, err := Verify(data, "test", fixture.roots); !errors.Is(err, ErrFormat) {
		t.Fatalf("keyless history became a runtime snapshot: %v", err)
	}
	if _, err := Sign(payload, fixture.certificate(1), fixture.online, fixture.roots); !errors.Is(err, ErrFormat) {
		t.Fatalf("new keyless publication accepted: %v", err)
	}
	aliases, err := history.RetainAliases(nil)
	if err != nil {
		t.Fatal(err)
	}
	upgraded := fixture.snapshot(t, fixture.payload(1, 3))
	if err := Accept(history.Cursor(), upgraded, fixture.now, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := RetainAliases(aliases, upgraded); err != nil {
		t.Fatal(err)
	}
	reassigned := fixture.payload(1, 4)
	reassigned.Nodes[0].Relay = "https://replacement.example"
	if _, err := RetainAliases(aliases, fixture.snapshot(t, reassigned)); !errors.Is(err, ErrAliasReassigned) {
		t.Fatal("archive lost alias ownership")
	}
	envelope.Signature = encoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	tampered, _ := json.Marshal(envelope)
	if _, err := VerifyHistory(tampered, "test", fixture.roots); !errors.Is(err, ErrSignature) {
		t.Fatal("unsigned archive accepted")
	}
}
