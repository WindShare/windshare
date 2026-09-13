package clievent

import (
	"testing"
	"time"
)

func TestRelayAvailabilityAndRecoveryContracts(t *testing.T) {
	visitor := &exhaustiveVisitor{}
	for _, args := range []struct {
		available, total, terminal uint32
		ready                      bool
	}{{0, 2, 0, false}, {1, 2, 0, true}, {0, 2, 2, true}} {
		event, err := NewRelayAvailability(args.available, args.total, args.terminal, args.ready)
		if err != nil || event.Available() != args.available || event.Total() != args.total || event.Terminal() != args.terminal || event.EverReady() != args.ready {
			t.Fatalf("availability=%+v err=%v", event, err)
		}
		if event.Command() != CommandShare || event.Level() != LevelInfo || event.Accept(visitor) != nil || event.Accept(nil) == nil {
			t.Fatal("availability event contract")
		}
	}
	if (RelayAvailability{}).Accept(visitor) == nil {
		t.Fatal("empty availability accepted")
	}
	for _, args := range []struct {
		a, t, p uint32
		r       bool
	}{{0, 0, 0, false}, {2, 1, 0, true}, {1, 2, 2, true}, {1, 1, 0, false}} {
		if _, err := NewRelayAvailability(args.a, args.t, args.p, args.r); err == nil {
			t.Fatalf("invalid availability accepted: %+v", args)
		}
	}
	authority, _ := NewRelayAuthority(RelayWSS, "relay.example", 443)
	event, err := NewRelayRecoveryObservation(CommandShare, authority, 2, RelayRecoveryWaiting, Failure{}, RelayRecoveryDetails{Generation: 3, Slow: true, NextDelay: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	details, ok := event.Details()
	if !ok || !details.Slow || details.Generation != 3 || event.Accept(visitor) != nil {
		t.Fatal("recovery decision context lost")
	}
	if _, err := NewRelayRecoveryObservation(CommandShare, authority, 0, RelayRecoveryWaiting, Failure{}, RelayRecoveryDetails{}); err == nil {
		t.Fatal("invalid attempt accepted")
	}
	if _, err := NewRelayRecoveryObservation(CommandShare, authority, 1, RelayRecoveryWaiting, Failure{}, RelayRecoveryDetails{NextDelay: -time.Second}); err == nil {
		t.Fatal("negative wait accepted")
	}
}
