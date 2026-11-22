package outputcap

import (
	"errors"
	"reflect"
	"testing"
)

func TestDestinationCapabilitiesSupportValidation(t *testing.T) {
	supported := SupportedCapability()
	unsafe, err := UnsupportedCapability(CapabilityReasonUnsafePublication)
	if err != nil {
		t.Fatal(err)
	}
	unrecoverable, err := UnsupportedCapability(CapabilityReasonUnverifiableOperationRecovery)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		publish   CapabilityEvidence
		operation CapabilityEvidence
		wantErr   error
	}{
		{name: "both supported", publish: supported, operation: supported},
		{name: "operation recovery missing", publish: supported, operation: unrecoverable, wantErr: ErrRecoverableOutputUnsupported},
		{name: "safe publish missing", publish: unsafe, operation: supported, wantErr: ErrOrdinaryOutputUnsupported},
		{name: "both missing", publish: unsafe, operation: unrecoverable, wantErr: ErrOrdinaryOutputUnsupported},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts, err := NewDestinationCapabilities(test.publish, test.operation)
			if err != nil {
				t.Fatal(err)
			}
			err = facts.ValidateSupport()
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateSupport() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("ValidateSupport() = %v, want %v", err, test.wantErr)
			}
		})
	}
	if err := (DestinationCapabilities{}).ValidateSupport(); !errors.Is(err, ErrInvalidDestinationCapabilities) {
		t.Fatalf("zero capabilities ValidateSupport() = %v", err)
	}
}

func TestCapabilityFactsAndPublishOutcomesAreClosed(t *testing.T) {
	if CapabilitySupported.String() != "supported" || CapabilityUnsupported.String() != "unsupported" ||
		CapabilityReasonUnverifiableOperationRecovery.String() != "operation-recovery-unverifiable" {
		t.Fatal("stable capability diagnostic vocabulary drifted")
	}
	for reason := CapabilityReasonNone; reason <= CapabilityReasonUnverifiableOperationRecovery; reason++ {
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
	unsupported, err := UnsupportedCapability(CapabilityReasonUnverifiableOperationRecovery)
	if err != nil {
		t.Fatal(err)
	}
	if unsupported.Fact() != CapabilityUnsupported ||
		unsupported.Reason() != CapabilityReasonUnverifiableOperationRecovery {
		t.Fatalf("unsupported evidence = (%v, %v)", unsupported.Fact(), unsupported.Reason())
	}
	capabilities, err := NewDestinationCapabilities(supported, unsupported)
	if err != nil || !capabilities.Valid() ||
		capabilities.SafePublish() != supported ||
		capabilities.OperationRecovery() != unsupported {
		t.Fatalf("capabilities = (%+v, %v)", capabilities, err)
	}
	if (DestinationCapabilities{}).Valid() {
		t.Fatal("zero destination capabilities became valid")
	}
	if CapabilityFact(255).String() != "" || PublishNoReplaceOutcome(255).String() != "" {
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
		reflect.TypeFor[CurrentEntryReference]():  {"AllocatedSize"},
		reflect.TypeFor[FileIdentity]():           {"ReadAt", "WriteAt", "Sync", "SetModifiedTime", "MetadataMatches", "Truncate", "AllocatedSize"},
		reflect.TypeFor[ObservedFile]():           {"WriteAt", "Sync", "SetModifiedTime", "Truncate", "AllocatedSize"},
		reflect.TypeFor[RecoveryDurabilityFile](): {"ReadAt", "WriteAt", "SetModifiedTime", "MetadataMatches", "Truncate", "AllocatedSize"},
		reflect.TypeFor[MutableFile]():            {"Truncate", "AllocatedSize"},
	}
	for capability, methods := range retired {
		for _, method := range methods {
			if _, found := capability.MethodByName(method); found {
				t.Errorf("%s still exposes retired method %s", capability, method)
			}
		}
	}
}
