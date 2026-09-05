package clievent

import "testing"

func TestPlatformSetupFactsAreClosedAndDispatchable(t *testing.T) {
	for _, state := range platformSetupStates[1:] {
		for _, reason := range platformSetupReasons[1:] {
			event, err := NewPlatformSetupObserved(CommandShare, state, reason)
			if err != nil || event.State() != state || event.Reason() != reason || event.Command() != CommandShare || event.Level() != LevelDebug {
				t.Fatalf("%+v: %v", event, err)
			}
			visitor := &exhaustiveVisitor{}
			if event.Accept(visitor) != nil || visitor.visited != "platform_setup" {
				t.Fatal(visitor)
			}
			if event.Accept(nil) == nil {
				t.Fatal("nil visitor accepted")
			}
		}
	}
	if _, err := NewPlatformSetupObserved(CommandGet, "configured", "attacker-controlled"); err == nil {
		t.Fatal("raw reason accepted")
	}
	if _, err := NewPlatformSetupObserved(0, "configured", "user-skipped"); err == nil {
		t.Fatal("missing command accepted")
	}
	if (PlatformSetupObserved{}).Accept(&exhaustiveVisitor{}) == nil {
		t.Fatal("zero event accepted")
	}
}
