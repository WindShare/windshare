package runtrace

import (
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestFilesystemCapabilitiesKeepSafeOutputSeparateFromRecovery(t *testing.T) {
	supported := clievent.FilesystemCapability{Supported: true, Reason: clievent.FilesystemCapabilityNone}
	capabilities := clievent.FilesystemDestinationCapabilities{
		Mode: clievent.FilesystemExecutionLiveOnly, SafePublish: supported,
		OperationRecovery: clievent.FilesystemCapability{Reason: clievent.FilesystemCapabilityOperationRecovery},
		RangeRecovery:     clievent.FilesystemCapability{Reason: clievent.FilesystemCapabilityRangeRecovery},
		CrashCleanup:      clievent.FilesystemCapability{Reason: clievent.FilesystemCapabilityCrashCleanup},
	}
	event, err := clievent.NewFilesystemOutputObserved(clievent.FilesystemOutputSpec{
		Operation: clievent.FilesystemRuntimeDecision, Capabilities: capabilities,
		RuntimeComponent: clievent.FilesystemRuntimeSession,
		RuntimeOperation: clievent.FilesystemRuntimeAdmitDestination,
		RuntimeDecision:  clievent.FilesystemRuntimeAdmitted,
	})
	if err != nil {
		t.Fatal(err)
	}
	record := &RunTraceRecordV3{}
	visitor := &encodeVisitorV3{record: record}
	if err := visitor.VisitFilesystemOutputObserved(event); err != nil {
		t.Fatal(err)
	}
	payload, ok := record.Payload.(filesystemOutputPayloadV3)
	if !ok || payload.Capabilities == nil {
		t.Fatalf("missing capability payload: %#v", record.Payload)
	}
	got := payload.Capabilities
	if got.Mode != "live-only" || !got.SafePublish.Supported || got.SafePublish.Reason != "none" ||
		got.OperationRecovery.Supported || got.OperationRecovery.Reason != "operation-recovery-unverifiable" ||
		got.RangeRecovery.Supported || got.RangeRecovery.Reason != "range-recovery-unverifiable" ||
		got.CrashCleanup.Supported || got.CrashCleanup.Reason != "crash-cleanup-unverifiable" ||
		payload.Failure != nil || payload.Certification != nil {
		t.Fatalf("capability degradation changed authority or became failure: %+v", payload)
	}
	capabilities.Mode = clievent.FilesystemExecutionResumable
	if _, err := clievent.NewFilesystemOutputObserved(clievent.FilesystemOutputSpec{
		Operation: clievent.FilesystemRuntimeDecision, Capabilities: capabilities,
	}); err == nil {
		t.Fatal("trace accepted unproven resumability")
	}
	capabilities.OperationRecovery, capabilities.RangeRecovery, capabilities.CrashCleanup = supported, supported, supported
	if _, err := clievent.NewFilesystemOutputObserved(clievent.FilesystemOutputSpec{
		Operation: clievent.FilesystemRuntimeDecision, Capabilities: capabilities,
	}); err != nil {
		t.Fatal(err)
	}
	capabilities.Mode = clievent.FilesystemExecutionMode(255)
	if capabilities.Valid() {
		t.Fatal("unknown execution mode accepted")
	}
	capabilities.Mode = clievent.FilesystemExecutionLiveOnly
	capabilities.SafePublish.Reason = clievent.FilesystemCapabilityReason(255)
	if capabilities.Valid() {
		t.Fatal("unknown capability reason accepted")
	}
}
