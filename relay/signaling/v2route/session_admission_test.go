package v2route

import (
	"errors"
	"testing"
	"time"
)

func TestSessionAdmissionDeadlineAndAuthority(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	registry := newRegistry(t, &now, &memoryTombstones{}, 2)
	fixture := makeFixture(t, 0x41)
	sender, receiver := routeTestConnection("sender"), routeTestConnection("receiver")
	publishRoute(t, registry, fixture, sender)
	joined, err := registry.Join(fixture.init.ShareID, receiver)
	if err != nil {
		t.Fatal(err)
	}
	id := joined.RelaySessionID
	replacement, err := NewConnectionRef("receiver")
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.AdmitSession(id, sender); !errors.Is(err, ErrSession) {
		t.Fatalf("admitted before receiver hello: %v", err)
	}
	if err := registry.ObserveReceiverFrame(id, replacement); !errors.Is(err, ErrOwner) {
		t.Fatalf("reused connection ID acquired admission: %v", err)
	}
	if err := registry.AdmitSession(id, receiver); !errors.Is(err, ErrOwner) {
		t.Fatal(err)
	}
	if _, _, expired := registry.ExpireAdmission(id, receiver); expired {
		t.Fatal("early expiry")
	}
	deadline := now.Add(SessionAdmissionTimeout)
	now = deadline.Add(-time.Nanosecond)
	if err := registry.ObserveReceiverFrame(id, receiver); err != nil {
		t.Fatal(err)
	}
	if registry.sessions[id].phase != SessionAwaitingSender {
		t.Fatal("first frame did not advance phase")
	}
	if registry.sessions[id].admissionDeadline != deadline {
		t.Fatal("traffic renewed admission deadline")
	}
	now = deadline
	if err := registry.ObserveReceiverFrame(id, receiver); !errors.Is(err, ErrAdmissionExpired) {
		t.Fatal(err)
	}
	if err := registry.AdmitSession(id, sender); !errors.Is(err, ErrAdmissionExpired) {
		t.Fatal(err)
	}
	if _, _, expired := registry.ExpireAdmission(id, replacement); expired {
		t.Fatal("stale generation expired session")
	}
	retired, phase, expired := registry.ExpireAdmission(id, receiver)
	if !expired || phase != SessionAwaitingSender || retired.RelaySessionID != id {
		t.Fatal(retired, phase, expired)
	}
	if registry.sessionSlotsByShare[fixture.init.ShareID] != 0 {
		t.Fatal("expired admission retained share slot")
	}
	if _, _, expired := registry.ExpireAdmission(id, receiver); expired {
		t.Fatal("duplicate expiry")
	}
	if err := registry.AdmitSession(id, sender); !errors.Is(err, ErrSessionEnded) {
		t.Fatal(err)
	}
	if err := registry.ObserveReceiverFrame(id, receiver); !errors.Is(err, ErrOwner) {
		t.Fatal(err)
	}
}

func TestAuthenticatedSessionCanRemainIdle(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	registry := newRegistry(t, &now, &memoryTombstones{}, 2)
	fixture := makeFixture(t, 0x42)
	sender, receiver := routeTestConnection("sender"), routeTestConnection("receiver")
	publishRoute(t, registry, fixture, sender)
	joined, err := registry.Join(fixture.init.ShareID, receiver)
	if err != nil {
		t.Fatal(err)
	}
	id := joined.RelaySessionID
	if err := registry.ObserveReceiverFrame(id, receiver); err != nil {
		t.Fatal(err)
	}
	if err := registry.AdmitSession(id, sender); err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	if _, _, expired := registry.ExpireAdmission(id, receiver); expired {
		t.Fatal("active session expired while idle")
	}
	if err := registry.AdmitSession(id, sender); err != nil {
		t.Fatal("confirmation is not idempotent", err)
	}
	if err := registry.ObserveReceiverFrame(id, receiver); err != nil {
		t.Fatal(err)
	}
	if !registry.sessions[id].admissionDeadline.IsZero() {
		t.Fatal("active session retained provisional deadline")
	}
}
