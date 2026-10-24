package clievent

import (
	"testing"

	appobs "github.com/windshare/windshare/engine/observation"
)

func TestPlatformSetupFactsAreClosedAndDispatchable(t *testing.T) {
	for _, state := range platformSetupStates[1:] {
		for _, reason := range platformSetupReasons[1:] {
			event, err := NewPlatformSetupObserved(CommandShare, state, reason)
			if err != nil || event.State() != state || event.Reason() != reason || event.Command() != CommandShare || event.Level() != appobs.LevelDebug {
				t.Fatalf("%+v: %v", event, err)
			}
			if event.Validate() != nil {
				t.Fatal("valid platform event rejected")
			}
		}
	}
	if _, err := NewPlatformSetupObserved(CommandGet, "configured", "attacker-controlled"); err == nil {
		t.Fatal("raw reason accepted")
	}
	if _, err := NewPlatformSetupObserved(0, "configured", "user-skipped"); err == nil {
		t.Fatal("missing command accepted")
	}
	if (PlatformSetupObserved{}).Validate() == nil {
		t.Fatal("zero event accepted")
	}
}
