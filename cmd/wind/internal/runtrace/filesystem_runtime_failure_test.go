package runtrace

import (
	"encoding/json"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestFilesystemRuntimeFailuresSurviveProjectionAndEncoding(t *testing.T) {
	value := fault.DependencyContractFault()
	tests := []struct {
		name      string
		component osfs.FilesystemOutputRuntimeComponent
		operation osfs.FilesystemOutputRuntimeOperation
		decision  osfs.FilesystemOutputRuntimeDecision
		hasFault  bool
	}{
		{"finalize tree", osfs.FilesystemOutputRuntimeSession, osfs.FilesystemOutputRuntimeFinalizeTree, osfs.FilesystemOutputRuntimeClosed, true},
		{"pause tree", osfs.FilesystemOutputRuntimeSession, osfs.FilesystemOutputRuntimePauseTree, osfs.FilesystemOutputRuntimeClosed, true},
		{"file retirement", osfs.FilesystemOutputRuntimeFile, osfs.FilesystemOutputRuntimeRetireFile, osfs.FilesystemOutputRuntimeNeedsAttention, true},
		{"directory ambiguity", osfs.FilesystemOutputRuntimeDirectory, osfs.FilesystemOutputRuntimeFinalizeDirectory, osfs.FilesystemOutputRuntimeAmbiguous, false},
		{"checkpoint cleanup", osfs.FilesystemOutputRuntimeCheckpoint, osfs.FilesystemOutputRuntimeCleanup, osfs.FilesystemOutputRuntimeCleanupPending, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := osfs.FilesystemOutputTrace{
				Operation: osfs.TraceRuntimeDecision, RuntimeComponent: test.component,
				RuntimeOperation: test.operation, RuntimeDecision: test.decision,
				OperationID: 17, Failed: true,
			}
			if test.hasFault {
				source.FaultDomain = uint8(value.Domain())
				source.NormalizedFaultScope = uint8(value.Scope())
				source.NormalizedFaultCode = value.Code()
			}
			event, err := commandprojection.ProjectFilesystemOutput(source)
			if err != nil {
				t.Fatalf("runtime failure was rejected: %v", err)
			}
			record := &RunTraceRecordV4{}
			visitor := &encodeVisitorV4{record: record}
			if err := visitor.VisitFilesystemOutputObserved(event); err != nil {
				t.Fatalf("runtime failure could not be encoded: %v", err)
			}
			payload, ok := record.Payload.(filesystemOutputPayloadV4)
			if !ok || payload.Failure == nil || payload.RuntimeDecision == nil || payload.Correlation == nil {
				t.Fatalf("failure or runtime context missing: %#v", record.Payload)
			}
			if payload.RuntimeDecision.Operation == "" || payload.RuntimeDecision.Component == "" ||
				payload.RuntimeDecision.Decision == "" || payload.Correlation.OperationID == nil ||
				*payload.Correlation.OperationID != "17" {
				t.Fatalf("runtime failure lost its operation: %+v", payload)
			}
			if test.hasFault && payload.Failure.Failure.Code != "session_dependency_contract" {
				t.Fatalf("runtime failure lost its cause: %+v", payload.Failure)
			}
			data, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var encoded struct {
				Failure map[string]json.RawMessage `json:"failure"`
			}
			if err := json.Unmarshal(data, &encoded); err != nil {
				t.Fatal(err)
			}
			if _, exists := encoded.Failure["stage"]; exists {
				t.Fatalf("runtime failure invented a native stage: %s", data)
			}
		})
	}
}
