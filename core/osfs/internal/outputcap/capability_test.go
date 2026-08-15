package outputcap

import (
	"errors"
	"reflect"
	"testing"
)

func TestExecutionModeReductionUsesFourOrthogonalFacts(t *testing.T) {
	supported := SupportedCapability()
	unsupported, err := UnsupportedCapability(CapabilityReasonUnverifiableRangeRecovery)
	if err != nil {
		t.Fatal(err)
	}
	unsafe, err := UnsupportedCapability(CapabilityReasonUnsafePublication)
	if err != nil {
		t.Fatal(err)
	}
	unclean, err := UnsupportedCapability(CapabilityReasonUnverifiableCrashCleanup)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		facts       [4]CapabilityEvidence
		mode        ExecutionMode
		unsupported bool
	}{
		{name: "all four resumable", facts: [4]CapabilityEvidence{supported, supported, supported, supported}, mode: ExecutionResumable},
		{name: "operation recovery missing", facts: [4]CapabilityEvidence{supported, unsupported, supported, supported}, mode: ExecutionLiveOnly},
		{name: "range recovery missing", facts: [4]CapabilityEvidence{supported, supported, unsupported, supported}, mode: ExecutionLiveOnly},
		{name: "both recovery facts missing", facts: [4]CapabilityEvidence{supported, unsupported, unsupported, supported}, mode: ExecutionLiveOnly},
		{name: "safe publish missing", facts: [4]CapabilityEvidence{unsafe, supported, supported, supported}, unsupported: true},
		{name: "crash cleanup missing", facts: [4]CapabilityEvidence{supported, supported, supported, unclean}, unsupported: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts, err := NewDestinationCapabilities(test.facts[0], test.facts[1], test.facts[2], test.facts[3])
			if err != nil {
				t.Fatal(err)
			}
			mode, err := SelectExecutionMode(facts)
			if test.unsupported {
				if mode != 0 || !errors.Is(err, ErrOrdinaryOutputUnsupported) {
					t.Fatalf("mode/error = %v/%v", mode, err)
				}
				return
			}
			if err != nil || mode != test.mode {
				t.Fatalf("mode/error = %v/%v, want %v/nil", mode, err, test.mode)
			}
		})
	}
	for mask := range 16 {
		facts := [4]CapabilityEvidence{unsupported, unsupported, unsupported, unsupported}
		for index := range facts {
			if mask&(1<<index) != 0 {
				facts[index] = supported
			}
		}
		capabilities, err := NewDestinationCapabilities(facts[0], facts[1], facts[2], facts[3])
		if err != nil {
			t.Fatal(err)
		}
		mode, err := SelectExecutionMode(capabilities)
		safePublish, operationRecovery, rangeRecovery, crashCleanup :=
			mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0
		switch {
		case safePublish && operationRecovery && rangeRecovery && crashCleanup:
			if err != nil || mode != ExecutionResumable {
				t.Fatalf("mask %04b = %v/%v, want resumable", mask, mode, err)
			}
		case safePublish && crashCleanup:
			if err != nil || mode != ExecutionLiveOnly {
				t.Fatalf("mask %04b = %v/%v, want live-only", mask, mode, err)
			}
		default:
			if mode != 0 || !errors.Is(err, ErrOrdinaryOutputUnsupported) {
				t.Fatalf("mask %04b = %v/%v, want unsupported", mask, mode, err)
			}
		}
	}
}

func TestCapabilityFactsAndPublishOutcomesAreClosed(t *testing.T) {
	if CapabilitySupported.String() != "supported" || CapabilityUnsupported.String() != "unsupported" ||
		ExecutionResumable.String() != "resumable" || ExecutionLiveOnly.String() != "live-only" ||
		CapabilityReasonCleanupOwnershipUnknown.String() != "cleanup-ownership-unknown" {
		t.Fatal("stable capability diagnostic vocabulary drifted")
	}
	for reason := CapabilityReasonNone; reason <= CapabilityReasonCleanupOwnershipUnknown; reason++ {
		if !reason.Valid() || reason.String() == "" {
			t.Fatalf("closed capability reason %d lacks a stable value", reason)
		}
	}
	if (CapabilityEvidence{}).Valid() {
		t.Fatal("zero capability evidence is valid")
	}
	if _, err := UnsupportedCapability(CapabilityReasonNone); !errors.Is(err, ErrInvalidDestinationCapabilities) {
		t.Fatalf("unsupported without reason = %v", err)
	}
	if _, err := UnsupportedCapability(CapabilityReason(255)); !errors.Is(err, ErrInvalidDestinationCapabilities) {
		t.Fatalf("unsupported with open reason = %v", err)
	}
	for _, outcome := range []PublishNoReplaceOutcome{
		PublishNoReplaceCommitted, PublishNoReplaceCollision, PublishNoReplaceIndeterminate,
	} {
		if !outcome.Valid() {
			t.Fatalf("closed publish outcome %v is invalid", outcome)
		}
		if outcome.String() == "" {
			t.Fatalf("publish outcome %v lacks a stable diagnostic value", outcome)
		}
	}
	if PublishNoReplaceOutcome(0).Valid() || PublishNoReplaceOutcome(4).Valid() {
		t.Fatal("publish outcome union is open")
	}
}

func TestCapabilityEvidenceAccessorsRetainOrthogonalFacts(t *testing.T) {
	supported := SupportedCapability()
	if supported.Fact() != CapabilitySupported || supported.Reason() != CapabilityReasonNone {
		t.Fatalf("supported evidence = (%v, %v)", supported.Fact(), supported.Reason())
	}
	unsupported, err := UnsupportedCapability(CapabilityReasonUnverifiableRangeRecovery)
	if err != nil {
		t.Fatal(err)
	}
	if unsupported.Fact() != CapabilityUnsupported ||
		unsupported.Reason() != CapabilityReasonUnverifiableRangeRecovery {
		t.Fatalf("unsupported evidence = (%v, %v)", unsupported.Fact(), unsupported.Reason())
	}
	capabilities, err := NewDestinationCapabilities(
		supported, unsupported, supported, supported,
	)
	if err != nil || !capabilities.Valid() ||
		capabilities.SafePublish() != supported ||
		capabilities.OperationRecovery() != unsupported ||
		capabilities.RangeRecovery() != supported ||
		capabilities.CrashCleanup() != supported {
		t.Fatalf("capabilities = (%+v, %v)", capabilities, err)
	}
	if (DestinationCapabilities{}).Valid() {
		t.Fatal("zero destination capabilities became valid")
	}
	if !ExecutionResumable.Valid() || !ExecutionLiveOnly.Valid() ||
		ExecutionMode(0).Valid() || ExecutionMode(255).Valid() {
		t.Fatal("execution mode union is open")
	}
	if CapabilityFact(255).String() != "" ||
		ExecutionMode(255).String() != "" ||
		PublishNoReplaceOutcome(255).String() != "" {
		t.Fatal("unknown diagnostics escaped the closed vocabulary")
	}
}

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
