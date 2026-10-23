package runtrace

import (
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestFilesystemCapabilitiesKeepSafeOutputSeparateFromRecovery(t *testing.T) {
	supported := appobs.FilesystemCapability{Supported: true, Reason: appobs.FilesystemCapabilityNone}
	capabilities := appobs.FilesystemDestinationCapabilities{
		Mode: appobs.FilesystemExecutionLiveOnly, SafePublish: supported,
		OperationRecovery: appobs.FilesystemCapability{Reason: appobs.FilesystemCapabilityOperationRecovery},
		RangeRecovery:     appobs.FilesystemCapability{Reason: appobs.FilesystemCapabilityRangeRecovery},
		CrashCleanup:      appobs.FilesystemCapability{Reason: appobs.FilesystemCapabilityCrashCleanup},
	}
	event, err := clievent.NewFilesystemOutputObserved(appobs.FilesystemOutputSpec{
		Operation: appobs.FilesystemRuntimeDecision, Capabilities: capabilities,
		RuntimeComponent: appobs.FilesystemRuntimeSession,
		RuntimeOperation: appobs.FilesystemRuntimeAdmitDestination,
		RuntimeDecision:  appobs.FilesystemRuntimeAdmitted,
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
	capabilities.Mode = appobs.FilesystemExecutionResumable
	if _, err := clievent.NewFilesystemOutputObserved(appobs.FilesystemOutputSpec{
		Operation: appobs.FilesystemRuntimeDecision, Capabilities: capabilities,
	}); err == nil {
		t.Fatal("trace accepted unproven resumability")
	}
	capabilities.OperationRecovery, capabilities.RangeRecovery, capabilities.CrashCleanup = supported, supported, supported
	if _, err := clievent.NewFilesystemOutputObserved(appobs.FilesystemOutputSpec{
		Operation: appobs.FilesystemRuntimeDecision, Capabilities: capabilities,
	}); err != nil {
		t.Fatal(err)
	}
	capabilities.Mode = appobs.FilesystemExecutionMode(255)
	if capabilities.Valid() {
		t.Fatal("unknown execution mode accepted")
	}
	capabilities.Mode = appobs.FilesystemExecutionLiveOnly
	capabilities.SafePublish.Reason = appobs.FilesystemCapabilityReason(255)
	if capabilities.Valid() {
		t.Fatal("unknown capability reason accepted")
	}
}
