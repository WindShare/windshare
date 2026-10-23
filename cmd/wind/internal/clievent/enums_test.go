package clievent

import (
	"testing"
)

func TestClosedEnumRegistriesNameEveryDeclaredValue(t *testing.T) {
	tests := []struct {
		name  string
		first int
		last  int
		value func(int) (string, bool)
	}{
		{"command", int(CommandShare), int(CommandGet), func(value int) (string, bool) { return Command(value).Name() }},
		{"exit", int(ExitSuccess), int(ExitDrift), func(value int) (string, bool) { return ExitCode(value).Name() }},
		{"result", int(ResultSuccess), int(ResultFailed), func(value int) (string, bool) { return ResultStatus(value).Name() }},
		{"drift", int(DriftNone), int(DriftSource), func(value int) (string, bool) { return DriftReason(value).Name() }},
		{"trace incomplete", int(TraceIncompleteLifecycleDrop), int(TraceIncompleteRejectionEvidence), func(value int) (string, bool) { return TraceIncompleteCause(value).Name() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for value := test.first; value <= test.last; value++ {
				name, ok := test.value(value)
				if !ok || name == "" {
					t.Fatalf("enum value %d has no stable name", value)
				}
			}
			for _, value := range []int{test.first - 1, test.last + 1} {
				if name, ok := test.value(value); ok || name != "" {
					t.Fatalf("unknown enum value %d mapped to %q", value, name)
				}
			}
		})
	}
	for code := ExitSuccess; code <= ExitDrift; code++ {
		if process, ok := code.ProcessCode(); !ok || process != int(code) || !code.Valid() {
			t.Fatalf("exit code %d process projection = %d,%t", code, process, ok)
		}
	}
	if _, ok := ExitCode(255).ProcessCode(); ok {
		t.Fatal("unknown exit code has a process code")
	}
}
