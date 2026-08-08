package outputcap

import (
	"reflect"
	"testing"
)

func TestRootOpenDispositionIsAClosedDurableSet(t *testing.T) {
	tests := []struct {
		name        string
		disposition RootOpenDisposition
		encoded     string
		valid       bool
	}{
		{
			name: "caller-provided container", disposition: CallerProvidedContainer,
			encoded: "caller-provided-container", valid: true,
		},
		{
			name: "authority-created root", disposition: AuthorityCreatedRoot,
			encoded: "authority-created-root", valid: true,
		},
		{name: "zero"},
		{name: "unknown", disposition: RootOpenDisposition("future-disposition")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.disposition.Valid(); got != test.valid {
				t.Fatalf("Valid() = %t, want %t", got, test.valid)
			}
			if test.valid && string(test.disposition) != test.encoded {
				t.Fatalf("durable encoding = %q, want %q", test.disposition, test.encoded)
			}
		})
	}
}

func TestCapabilityInterfacesExcludeRetiredAdapterMethods(t *testing.T) {
	retired := map[reflect.Type][]string{
		reflect.TypeFor[Directory](): {
			"NamesWithPrefix", "ValidatePublicEntryName", "PrepareIdentityClaim", "IdentityClaim",
		},
		reflect.TypeFor[CurrentEntryReference](): {"AllocatedSize"},
		reflect.TypeFor[File]():                  {"Truncate", "AllocatedSize"},
	}
	for capability, methods := range retired {
		for _, method := range methods {
			if _, found := capability.MethodByName(method); found {
				t.Errorf("%s still exposes retired method %s", capability, method)
			}
		}
	}
}
