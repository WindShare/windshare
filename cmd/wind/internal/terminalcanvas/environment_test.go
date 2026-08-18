package terminalcanvas

import (
	"testing"
	"time"
)

func TestNativeEnvironmentKeepsRawOutputAndClockSeparate(t *testing.T) {
	raw := fakeRawTerminal(^uintptr(0))
	environment := NewNativeEnvironment(raw)

	if environment.RawOutput != raw {
		t.Fatalf("RawOutput = %v, want injected raw terminal", environment.RawOutput)
	}
	capabilities := environment.Capabilities.Snapshot()
	if capabilities.Interactive || capabilities.ANSI || capabilities.Color || capabilities.Unicode || capabilities.Columns != 0 {
		t.Fatalf("invalid native handle produced capabilities: %+v", capabilities)
	}
	before := time.Now()
	now := environment.Clock.Now()
	after := time.Now()
	if now.Before(before) || now.After(after) {
		t.Fatalf("clock returned %v outside [%v, %v]", now, before, after)
	}
}

func TestNativeEnvironmentNilRawTerminalIsConservative(t *testing.T) {
	environment := NewNativeEnvironment(nil)
	if got := environment.Capabilities.Snapshot(); got != (Capabilities{}) {
		t.Fatalf("Snapshot() = %+v, want zero capabilities", got)
	}
}

func TestNilCapabilityProviderFuncIsConservative(t *testing.T) {
	var provider CapabilityProviderFunc
	if got := provider.Snapshot(); got != (Capabilities{}) {
		t.Fatalf("Snapshot() = %+v, want zero capabilities", got)
	}
}

func TestNativeColorPolicyHonorsNOColorIndependently(t *testing.T) {
	lookup := func(name string) (string, bool) {
		if name == "NO_COLOR" {
			return "", true
		}
		return "", false
	}
	if nativeColorEnabled(true, lookup) {
		t.Fatal("NO_COLOR did not disable color")
	}
	if nativeColorEnabled(false, func(string) (string, bool) { return "", false }) {
		t.Fatal("color enabled without ANSI")
	}
	if !nativeColorEnabled(true, func(string) (string, bool) { return "", false }) {
		t.Fatal("color disabled despite ANSI and absent NO_COLOR")
	}
}

func TestLocaleUnicodePolicyUsesFirstConfiguredLocale(t *testing.T) {
	values := map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"}
	lookup := func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
	if localeSupportsUnicode(lookup) {
		t.Fatal("LC_ALL=C must override a UTF-8 LANG")
	}
	delete(values, "LC_ALL")
	if !localeSupportsUnicode(lookup) {
		t.Fatal("UTF-8 LANG was not recognized")
	}
}

type fakeRawTerminal uintptr

func (raw fakeRawTerminal) Fd() uintptr {
	return uintptr(raw)
}
