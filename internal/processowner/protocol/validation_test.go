package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/windshare/windshare/internal/testtrace"
)

func TestValidateRequestRejectsEveryNoncanonicalBoundary(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "schema", mutate: func(request *Request) { request.SchemaVersion = "unsupported" }},
		{name: "run id", mutate: func(request *Request) { request.RunID = "" }},
		{name: "operation NUL", mutate: func(request *Request) { request.OperationID = "bad\x00id" }},
		{name: "scenario NFC", mutate: func(request *Request) { request.Scenario = "e\u0301" }},
		{name: "deadline low", mutate: func(request *Request) { request.DeadlineMilliseconds = 0 }},
		{name: "deadline high", mutate: func(request *Request) { request.DeadlineMilliseconds = MaximumDeadlineMilliseconds + 1 }},
		{name: "grace low", mutate: func(request *Request) { request.TerminationGraceMilliseconds = 0 }},
		{name: "grace high", mutate: func(request *Request) { request.TerminationGraceMilliseconds = MaximumTerminationMilliseconds + 1 }},
		{name: "executable relative", mutate: func(request *Request) { request.Command.Executable = "fixture" }},
		{name: "executable NUL", mutate: func(request *Request) { request.Command.Executable += "\x00" }},
		{name: "working directory relative", mutate: func(request *Request) { request.Command.WorkingDirectory = "." }},
		{name: "arguments nil", mutate: func(request *Request) { request.Command.Arguments = nil }},
		{name: "argument NUL", mutate: func(request *Request) { request.Command.Arguments = []string{"bad\x00argument"} }},
		{name: "environment nil", mutate: func(request *Request) { request.Command.Environment = nil }},
		{name: "environment name", mutate: func(request *Request) {
			request.Command.Environment = []EnvironmentEntry{{Name: "A=B", Value: "value"}}
		}},
		{name: "environment value", mutate: func(request *Request) {
			request.Command.Environment = []EnvironmentEntry{{Name: "A", Value: "bad\x00value"}}
		}},
		{name: "reserved event environment", mutate: func(request *Request) {
			request.Command.Environment = []EnvironmentEntry{{Name: testtrace.EventFDEnvironment, Value: "7"}}
		}},
		{name: "stdin empty", mutate: func(request *Request) { request.Command.Stdin = &Stdin{} }},
		{name: "stdin oversized", mutate: func(request *Request) {
			request.Command.Stdin = &Stdin{ByteLength: MaximumStdinBytes + 1}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validRequest(t)
			test.mutate(&request)
			if err := ValidateRequest(request); err == nil {
				t.Fatal("invalid request was accepted")
			}
		})
	}
}

func TestValidateRequestRejectsNullArgumentsFromCanonicalWire(t *testing.T) {
	request := validRequest(t)
	request.Command.Arguments = nil
	encoded, err := EncodeCanonical(request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"arguments":null`)) {
		t.Fatalf("hostile request did not encode a null arguments field: %s", encoded)
	}
	decoded, err := DecodeCanonical[Request](encoded)
	if err != nil {
		t.Fatalf("null arguments wire fixture is not canonical JSON: %v", err)
	}
	if err := ValidateRequest(decoded); err == nil {
		t.Fatal("canonical wire request with null arguments was accepted")
	}
}

func TestValidateControlRequiresExactIdentityAndReason(t *testing.T) {
	identity := validRequest(t).Identity
	control := Control{SchemaVersion: ControlSchemaVersion, Identity: identity, Reason: ControlReasonStop}
	if err := ValidateControl(control, identity); err != nil {
		t.Fatal(err)
	}
	control.Reason = ControlReasonParentLost
	if err := ValidateControl(control, identity); err != nil {
		t.Fatal(err)
	}
	control.Reason = ControlReasonDeadline
	if err := ValidateControl(control, identity); err != nil {
		t.Fatal(err)
	}
	control.SchemaVersion = "unsupported"
	if err := ValidateControl(control, identity); err == nil {
		t.Fatal("unsupported control schema was accepted")
	}
	control.SchemaVersion = ControlSchemaVersion
	control.OperationID = "different"
	if err := ValidateControl(control, identity); err == nil {
		t.Fatal("mismatched control identity was accepted")
	}
	control.Identity = identity
	control.Reason = "kill"
	if err := ValidateControl(control, identity); err == nil {
		t.Fatal("unsupported control reason was accepted")
	}
}

func TestValidateSettlementRejectsInconsistentEvidence(t *testing.T) {
	valid := validSettlement(t)
	if err := ValidateSettlement(valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Settlement)
	}{
		{name: "schema", mutate: func(value *Settlement) { value.SchemaVersion = "unsupported" }},
		{name: "identity", mutate: func(value *Settlement) { value.RunID = "" }},
		{name: "target outcome", mutate: func(value *Settlement) { value.Target.Outcome = "unknown" }},
		{name: "termination", mutate: func(value *Settlement) { value.TerminationReason = "unknown" }},
		{name: "input", mutate: func(value *Settlement) { value.Input.Outcome = "unknown" }},
		{name: "process diagnostic", mutate: func(value *Settlement) {
			value.Target.FailureMessage = strings.Repeat("x", MaximumDiagnosticBytes+1)
		}},
		{name: "input diagnostic", mutate: func(value *Settlement) {
			value.Input.FailureMessage = strings.Repeat("x", MaximumDiagnosticBytes+1)
		}},
		{name: "cleanup diagnostic", mutate: func(value *Settlement) {
			value.Cleanup.FailureMessage = strings.Repeat("x", MaximumDiagnosticBytes+1)
		}},
		{name: "completed failure", mutate: func(value *Settlement) { value.Cleanup.FailureCode = "FAIL" }},
		{name: "failed lacks evidence", mutate: func(value *Settlement) {
			value.TreeState = TreeUnknown
			value.Cleanup = CleanupEvidence{Outcome: CleanupFailed}
		}},
		{name: "cleanup outcome", mutate: func(value *Settlement) { value.Cleanup.Outcome = "unknown" }},
		{name: "platform", mutate: func(value *Settlement) { value.Platform.Kind = "unknown" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settlement := valid
			test.mutate(&settlement)
			if err := ValidateSettlement(settlement); err == nil {
				t.Fatal("inconsistent settlement was accepted")
			}
		})
	}
	deadline := valid
	deadline.TerminationReason = TerminationDeadline
	if err := ValidateSettlement(deadline); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSettlementTruthTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Settlement)
		wantErr bool
	}{
		{
			name: "deadline plus owner cleanup failure after proven empty",
			mutate: func(value *Settlement) {
				value.TerminationReason = TerminationDeadline
				setCleanupFailure(value, TreeProvenEmpty)
			},
		},
		{
			name: "known started terminal lost without root identity",
			mutate: func(value *Settlement) {
				value.TerminationReason = TerminationOwnerFailure
				value.Target = lostTarget(TargetTerminalEvidenceLost)
				value.OwnerFailure = testOwnerFailure()
				value.Platform.Root = nil
				value.Platform.RootStartTimeTicks = ""
				value.Platform.InventoryScans = 0
				value.Platform.QuietInventoryCount = 0
			},
		},
		{
			name: "start evidence lost after Linux gate exited",
			mutate: func(value *Settlement) {
				value.TerminationReason = TerminationOwnerFailure
				value.Target = lostTarget(TargetStartEvidenceLost)
				value.OwnerFailure = testOwnerFailure()
			},
		},
		{
			name: "target not started after Linux gate exited",
			mutate: func(value *Settlement) {
				value.TerminationReason = TerminationStop
				value.Target = failedTarget(TargetNotStarted)
			},
		},
		{
			name: "prelaunch Linux gate remains nonempty",
			mutate: func(value *Settlement) {
				value.TerminationReason = TerminationStop
				value.Target = failedTarget(TargetNotStarted)
				value.Platform.Root.State = RootActive
				value.Platform.Root.ExitCode = nil
				active := uint32(1)
				value.Platform.ActiveProcessCount = &active
				setCleanupFailure(value, TreeNonempty)
			},
		},
		{
			name: "unknown tree with lost Linux root terminal",
			mutate: func(value *Settlement) {
				value.TerminationReason = TerminationOwnerFailure
				value.Target = lostTarget(TargetStartEvidenceLost)
				value.Platform.Root.State = RootTerminalEvidenceLost
				value.Platform.Root.ExitCode = nil
				setCleanupFailure(value, TreeUnknown)
			},
		},
		{
			name: "exact target exit without root",
			mutate: func(value *Settlement) {
				value.Platform.Root = nil
				value.Platform.RootStartTimeTicks = ""
			},
			wantErr: true,
		},
		{
			name: "lost target terminal contradicts exact root",
			mutate: func(value *Settlement) {
				value.TerminationReason = TerminationOwnerFailure
				value.Target = lostTarget(TargetTerminalEvidenceLost)
				value.OwnerFailure = testOwnerFailure()
			},
			wantErr: true,
		},
		{
			name: "lost root terminal lacks owner failure",
			mutate: func(value *Settlement) {
				value.TerminationReason = TerminationStop
				value.Target = failedTarget(TargetNotStarted)
				value.Platform.Root.State = RootTerminalEvidenceLost
				value.Platform.Root.ExitCode = nil
			},
			wantErr: true,
		},
		{
			name: "proven empty contradicts active root",
			mutate: func(value *Settlement) {
				value.TerminationReason = TerminationStop
				value.Target = failedTarget(TargetNotStarted)
				value.Platform.Root.State = RootActive
				value.Platform.Root.ExitCode = nil
			},
			wantErr: true,
		},
		{
			name: "Windows root contradicts target not created",
			mutate: func(value *Settlement) {
				value.TerminationReason = TerminationInitializationFailed
				value.Target = failedTarget(TargetSpawnFailed)
				makeWindowsEvidence(value)
			},
			wantErr: true,
		},
		{
			name: "Windows rejects signal terminal",
			mutate: func(value *Settlement) {
				value.Target = TargetEvidence{Outcome: TargetSignaled, Signal: "SIGTERM"}
				value.Platform.Root.State = RootSignaled
				value.Platform.Root.ExitCode = nil
				value.Platform.Root.Signal = "SIGTERM"
				makeWindowsEvidence(value)
			},
			wantErr: true,
		},
		{
			name: "Windows rejects exit beyond DWORD",
			mutate: func(value *Settlement) {
				exitCode := int64(1) << 32
				value.Target.ExitCode = &exitCode
				value.Platform.Root.ExitCode = &exitCode
				makeWindowsEvidence(value)
			},
			wantErr: true,
		},
		{
			name: "start evidence lost requires owner failure",
			mutate: func(value *Settlement) {
				value.TerminationReason = TerminationInitializationFailed
				value.Target = lostTarget(TargetStartEvidenceLost)
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settlement := validSettlement(t)
			test.mutate(&settlement)
			err := ValidateSettlement(settlement)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateSettlement error = %v, settlement=%#v", err, settlement)
			}
		})
	}
}

func TestValidateSettlementForRequestBindsInputEvidence(t *testing.T) {
	request := validRequest(t)
	settlement := validSettlement(t)
	if err := ValidateSettlementForRequest(settlement, request); err != nil {
		t.Fatal(err)
	}
	settlement.Input = InputEvidence{Outcome: InputDelivered}
	if err := ValidateSettlementForRequest(settlement, request); err == nil {
		t.Fatal("input-free request accepted delivered input evidence")
	}

	request.Command.Stdin = &Stdin{ByteLength: 1}
	settlement = validSettlement(t)
	if err := ValidateSettlementForRequest(settlement, request); err == nil {
		t.Fatal("declared input accepted not-requested evidence")
	}
	settlement.Input = InputEvidence{Outcome: InputDelivered}
	if err := ValidateSettlementForRequest(settlement, request); err != nil {
		t.Fatal(err)
	}
	settlement.TerminationReason = TerminationStop
	settlement.Target = failedTarget(TargetNotStarted)
	settlement.Input = InputEvidence{Outcome: InputNotStarted}
	if err := ValidateSettlementForRequest(settlement, request); err != nil {
		t.Fatal(err)
	}
	settlement.TerminationReason = TerminationOwnerFailure
	settlement.Target = lostTarget(TargetStartEvidenceLost)
	settlement.OwnerFailure = testOwnerFailure()
	if err := ValidateSettlementForRequest(settlement, request); err != nil {
		t.Fatal(err)
	}
	settlement.Input = InputEvidence{
		Outcome: InputEvidenceLost, FailureCode: "INPUT_EVIDENCE_LOST", FailureMessage: "input boundary was lost",
	}
	if err := ValidateSettlementForRequest(settlement, request); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalDocumentAndFrameRejectAmbiguousBytes(t *testing.T) {
	request := validRequest(t)
	encoded, err := EncodeCanonical(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadDocument[Request](bytes.NewReader(encoded))
	if err != nil || decoded.Identity != request.Identity {
		t.Fatalf("ReadDocument = %#v, %v", decoded, err)
	}
	if err := WriteDocument(io.Discard, request); err != nil {
		t.Fatal(err)
	}
	for name, malformed := range map[string][]byte{
		"empty":         nil,
		"whitespace":    append([]byte(" "), encoded...),
		"trailing":      append(bytes.Clone(encoded), []byte("{}")...),
		"unknown field": append(bytes.TrimSuffix(bytes.Clone(encoded), []byte("}")), []byte(",\"unknown\":true}")...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCanonical[Request](malformed); err == nil {
				t.Fatal("ambiguous JSON was accepted")
			}
		})
	}
	oversized := bytes.Repeat([]byte("x"), MaximumDocumentBytes+1)
	if _, err := ReadDocument[Request](bytes.NewReader(oversized)); err == nil {
		t.Fatal("oversized document was accepted")
	}

	if _, err := ReadFrame[Request](bytes.NewReader([]byte{0, 0, 0, 0})); err == nil {
		t.Fatal("zero frame was accepted")
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, MaximumDocumentBytes+1)
	if _, err := ReadFrame[Request](bytes.NewReader(header)); err == nil {
		t.Fatal("oversized frame was accepted")
	}
	binary.BigEndian.PutUint32(header, 5)
	if _, err := ReadFrame[Request](bytes.NewReader(append(header, []byte("{}")...))); err == nil {
		t.Fatal("short frame was accepted")
	}
	if err := WriteFrame(zeroWriter{}, request); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero writer error = %v", err)
	}
}

func TestValidateEventRejectsMalformedTraceRecords(t *testing.T) {
	event := Event{
		SchemaVersion: EventSchemaVersion, Identity: validRequest(t).Identity,
		Component: "fixture", Milestone: "ready", Outcome: "succeeded", Payload: json.RawMessage(`{"ready":true}`),
	}
	if err := ValidateEvent(event); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Event)
	}{
		{name: "schema", mutate: func(value *Event) { value.SchemaVersion = "unsupported" }},
		{name: "identity", mutate: func(value *Event) { value.Scenario = "" }},
		{name: "component", mutate: func(value *Event) { value.Component = "" }},
		{name: "milestone", mutate: func(value *Event) { value.Milestone = "invalid/milestone" }},
		{name: "outcome", mutate: func(value *Event) { value.Outcome = "bad\x00outcome" }},
		{name: "payload", mutate: func(value *Event) { value.Payload = json.RawMessage(`{`) }},
		{name: "oversized payload", mutate: func(value *Event) { value.Payload = bytes.Repeat([]byte("1"), MaximumDocumentBytes+1) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := event
			test.mutate(&candidate)
			if err := ValidateEvent(candidate); err == nil {
				t.Fatal("invalid event was accepted")
			}
		})
	}
}

func TestDiagnosticValidationRetainsUnicodeBoundaries(t *testing.T) {
	invalid := string([]byte{utf8.RuneSelf})
	if err := validateOptionalDiagnostic("diagnostic", invalid); err == nil {
		t.Fatal("invalid UTF-8 diagnostic was accepted")
	}
	if err := validateOptionalDiagnostic("diagnostic", "e\u0301"); err == nil {
		t.Fatal("non-NFC diagnostic was accepted")
	}
	if err := validateOptionalDiagnostic("diagnostic", "embedded\x00NUL"); err == nil {
		t.Fatal("NUL diagnostic was accepted")
	}
}

func validSettlement(t *testing.T) Settlement {
	t.Helper()
	exitCode := int64(0)
	active := uint32(0)
	return Settlement{
		SchemaVersion: SettlementSchemaVersion, Identity: validRequest(t).Identity,
		TerminationReason: TerminationNatural,
		Target:            TargetEvidence{Outcome: TargetExited, ExitCode: &exitCode},
		Input:             InputEvidence{Outcome: InputNotRequested},
		TreeState:         TreeProvenEmpty,
		Cleanup:           CleanupEvidence{Outcome: CleanupCompleted},
		Platform: PlatformEvidence{
			Kind: PlatformLinuxSubreaper, OwnerPID: os.Getpid(),
			Root: &RootEvidence{PID: 1, State: RootExited, ExitCode: &exitCode}, RootStartTimeTicks: "1",
			ActiveProcessCount: &active, InventoryScans: 2, QuietInventoryCount: 2,
		},
	}
}

func failedTarget(outcome string) TargetEvidence {
	return TargetEvidence{
		Outcome: outcome, FailureCode: "TARGET_NOT_STARTED", FailureMessage: "target did not start",
	}
}

func lostTarget(outcome string) TargetEvidence {
	return TargetEvidence{
		Outcome: outcome, FailureCode: "TARGET_EVIDENCE_LOST", FailureMessage: "target evidence was lost",
	}
}

func testOwnerFailure() *FailureEvidence {
	return &FailureEvidence{Code: "OWNER_FAILED", Message: "owner evidence failed"}
}

func setCleanupFailure(settlement *Settlement, treeState string) {
	settlement.TreeState = treeState
	settlement.OwnerFailure = testOwnerFailure()
	settlement.Cleanup = CleanupEvidence{
		Outcome: CleanupFailed, FailureCode: "CLEANUP_FAILED", FailureMessage: "cleanup evidence failed",
	}
	switch treeState {
	case TreeProvenEmpty:
		active := uint32(0)
		settlement.Platform.ActiveProcessCount = &active
	case TreeNonempty:
		active := uint32(1)
		settlement.Platform.ActiveProcessCount = &active
	case TreeUnknown:
		settlement.Platform.ActiveProcessCount = nil
	}
}

func makeWindowsEvidence(settlement *Settlement) {
	settlement.Platform.Kind = PlatformWindowsJob
	settlement.Platform.RootStartTimeTicks = ""
	settlement.Platform.InventoryScans = 0
	settlement.Platform.MaximumObservedDescendants = 0
	settlement.Platform.QuietInventoryCount = 0
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

var _ io.Writer = zeroWriter{}

func TestValidateCommandRejectsNoncanonicalAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	command := Command{
		Executable: root + string(os.PathSeparator) + "folder" + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "fixture", Arguments: []string{},
		WorkingDirectory: root, Environment: []EnvironmentEntry{},
	}
	if err := ValidateCommand(command); err == nil {
		t.Fatal("unclean executable path was accepted")
	}
}
