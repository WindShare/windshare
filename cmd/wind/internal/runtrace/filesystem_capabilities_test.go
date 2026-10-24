package runtrace

import (
	"testing"

	fsobs "github.com/windshare/windshare/adapters/filesystem/diagnostic"
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestFilesystemCapabilitiesKeepSafeOutputSeparateFromRecovery(t *testing.T) {
	supported := fsobs.FilesystemCapability{Supported: true, Reason: fsobs.FilesystemCapabilityNone}
	capabilities := fsobs.FilesystemDestinationCapabilities{
		Mode: fsobs.FilesystemExecutionLiveOnly, SafePublish: supported,
		OperationRecovery: fsobs.FilesystemCapability{Reason: fsobs.FilesystemCapabilityOperationRecovery},
		RangeRecovery:     fsobs.FilesystemCapability{Reason: fsobs.FilesystemCapabilityRangeRecovery},
		CrashCleanup:      fsobs.FilesystemCapability{Reason: fsobs.FilesystemCapabilityCrashCleanup},
	}
	event, err := clievent.NewFilesystemOutputObserved(fsobs.FilesystemOutputSpec{
		Operation: fsobs.FilesystemRuntimeDecision, Capabilities: capabilities,
		RuntimeComponent: fsobs.FilesystemRuntimeSession,
		RuntimeOperation: fsobs.FilesystemRuntimeAdmitDestination,
		RuntimeDecision:  fsobs.FilesystemRuntimeAdmitted,
	})
	if err != nil {
		t.Fatal(err)
	}
	record := &RunTraceRecordV4{}
	visitor := &encodeVisitorV4{record: record}
	if err := visitor.VisitFilesystemOutputObserved(event); err != nil {
		t.Fatal(err)
	}
	payload, ok := record.Payload.(filesystemOutputPayloadV4)
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
	capabilities.Mode = fsobs.FilesystemExecutionResumable
	if _, err := clievent.NewFilesystemOutputObserved(fsobs.FilesystemOutputSpec{
		Operation: fsobs.FilesystemRuntimeDecision, Capabilities: capabilities,
	}); err == nil {
		t.Fatal("trace accepted unproven resumability")
	}
	capabilities.OperationRecovery, capabilities.RangeRecovery, capabilities.CrashCleanup = supported, supported, supported
	if _, err := clievent.NewFilesystemOutputObserved(fsobs.FilesystemOutputSpec{
		Operation: fsobs.FilesystemRuntimeDecision, Capabilities: capabilities,
	}); err != nil {
		t.Fatal(err)
	}
	capabilities.Mode = fsobs.FilesystemExecutionMode(255)
	if capabilities.Valid() {
		t.Fatal("unknown execution mode accepted")
	}
	capabilities.Mode = fsobs.FilesystemExecutionLiveOnly
	capabilities.SafePublish.Reason = fsobs.FilesystemCapabilityReason(255)
	if capabilities.Valid() {
		t.Fatal("unknown capability reason accepted")
	}
}
