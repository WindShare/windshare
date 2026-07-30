package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

const testNonce = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestCanonicalJSONMatchesJSONStringifyEscaping(t *testing.T) {
	t.Parallel()
	value := struct {
		Text string `json:"text"`
	}{Text: "<>&" + "\u2028\u2029" + `\u2028\` + "\u2028"}
	encoded, err := canonicalJSON(value)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	expected := []byte(`{"text":"<>&` + "\u2028\u2029" + `\\u2028\\` + "\u2028" + `"}`)
	if !bytes.Equal(encoded, expected) {
		t.Fatalf("canonical JSON = %q, want %q", encoded, expected)
	}
	if bytes.HasSuffix(encoded, []byte{'\n'}) {
		t.Fatal("canonical JSON has trailing LF")
	}
}

func TestCanonicalFrameRoundTripAndRejectsAlternateEncoding(t *testing.T) {
	t.Parallel()
	request := validStartRequest(t)
	var framed bytes.Buffer
	if err := writeCanonicalFrame(&framed, request); err != nil {
		t.Fatalf("writeCanonicalFrame: %v", err)
	}
	decoded, err := readCanonicalFrame[startRequest](&framed, "request")
	if err != nil {
		t.Fatalf("readCanonicalFrame: %v", err)
	}
	if decoded.OperationID != request.OperationID || decoded.Arguments[0] != request.Arguments[0] {
		t.Fatalf("decoded request = %#v", decoded)
	}

	canonical, err := canonicalJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	noncanonical := append([]byte{' '}, canonical...)
	var invalid bytes.Buffer
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(noncanonical)))
	invalid.Write(header)
	invalid.Write(noncanonical)
	if _, err := readCanonicalFrame[startRequest](&invalid, "request"); err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("noncanonical frame error = %v", err)
	}
}

func TestCanonicalFrameRejectsUnknownDuplicateAndTruncatedInput(t *testing.T) {
	t.Parallel()
	request := validStartRequest(t)
	canonical, err := canonicalJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		encoded []byte
	}{
		{name: "unknown", encoded: bytes.Replace(canonical, []byte(`"schemaVersion":2`), []byte(`"unknown":0,"schemaVersion":2`), 1)},
		{name: "duplicate", encoded: bytes.Replace(canonical, []byte(`"schemaVersion":2`), []byte(`"schemaVersion":2,"schemaVersion":2`), 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var framed bytes.Buffer
			header := make([]byte, 4)
			binary.BigEndian.PutUint32(header, uint32(len(test.encoded)))
			framed.Write(header)
			framed.Write(test.encoded)
			if _, err := readCanonicalFrame[startRequest](&framed, "request"); err == nil {
				t.Fatal("invalid frame was accepted")
			}
		})
	}
	truncated := bytes.NewReader([]byte{0, 0, 0, 3, '{'})
	if _, err := readCanonicalFrame[startRequest](truncated, "request"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated error = %v", err)
	}
}

func TestStartRequestRequiresExplicitCanonicalArraysAndEnvironment(t *testing.T) {
	t.Parallel()
	valid := validStartRequest(t)
	tests := []struct {
		name   string
		mutate func(*startRequest)
	}{
		{name: "schema", mutate: func(request *startRequest) { request.SchemaVersion++ }},
		{name: "type", mutate: func(request *startRequest) { request.Type = "other" }},
		{name: "empty operation ID", mutate: func(request *startRequest) { request.OperationID = "" }},
		{name: "oversized operation ID", mutate: func(request *startRequest) {
			request.OperationID = strings.Repeat("x", maximumOperationBytes+1)
		}},
		{name: "operation ID NUL", mutate: func(request *startRequest) { request.OperationID += "\x00" }},
		{name: "nil arguments", mutate: func(request *startRequest) { request.Arguments = nil }},
		{name: "nil environment", mutate: func(request *startRequest) { request.Environment = nil }},
		{name: "relative executable", mutate: func(request *startRequest) { request.Executable = "target.exe" }},
		{name: "executable NUL", mutate: func(request *startRequest) { request.Executable += "\x00" }},
		{name: "executable digest", mutate: func(request *startRequest) { request.ExecutableSHA256 = "ABC" }},
		{name: "relative cwd", mutate: func(request *startRequest) { request.CWD = "work" }},
		{name: "cwd NUL", mutate: func(request *startRequest) { request.CWD += "\x00" }},
		{name: "operation ID is not NFC", mutate: func(request *startRequest) { request.OperationID = "e\u0301" }},
		{name: "nonce length", mutate: func(request *startRequest) { request.Nonce = "00" }},
		{name: "nonce uppercase", mutate: func(request *startRequest) { request.Nonce = strings.ToUpper(testNonce) }},
		{name: "nonce non-hex", mutate: func(request *startRequest) { request.Nonce = strings.Repeat("g", nonceEncodedBytes) }},
		{name: "argument NUL", mutate: func(request *startRequest) { request.Arguments = []string{"bad\x00argument"} }},
		{name: "deadline below minimum", mutate: func(request *startRequest) { request.DeadlineMS = minimumDeadlineMS - 1 }},
		{name: "deadline above maximum", mutate: func(request *startRequest) { request.DeadlineMS = maximumDeadlineMS + 1 }},
		{name: "grace below minimum", mutate: func(request *startRequest) { request.TerminationGraceMS = minimumGraceMS - 1 }},
		{name: "grace above maximum", mutate: func(request *startRequest) { request.TerminationGraceMS = maximumGraceMS + 1 }},
		{name: "empty environment name", mutate: func(request *startRequest) {
			request.Environment = []environmentEntry{{Name: "", Value: "value"}}
		}},
		{name: "environment name equals", mutate: func(request *startRequest) {
			request.Environment = []environmentEntry{{Name: "BAD=NAME", Value: "value"}}
		}},
		{name: "environment value NUL", mutate: func(request *startRequest) {
			request.Environment = []environmentEntry{{Name: "NAME", Value: "bad\x00value"}}
		}},
		{name: "case duplicate", mutate: func(request *startRequest) {
			request.Environment = []environmentEntry{{Name: "Path", Value: "one"}, {Name: "PATH", Value: "two"}}
		}},
		{name: "unsorted", mutate: func(request *startRequest) {
			request.Environment = []environmentEntry{{Name: "Z", Value: "one"}, {Name: "a", Value: "two"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := valid
			test.mutate(&request)
			if err := validateStartRequest(request); err == nil {
				t.Fatal("invalid request was accepted")
			}
		})
	}
}

func TestOptionalRawStdinContainsOnlyBoundedNonsecretAuthority(t *testing.T) {
	t.Parallel()
	input := &rawStdin{
		Kind: rawStdinKind, Descriptor: 0, ByteLength: 3,
		ChannelID: "channel", RunID: "run", ProfileID: "profile", AttemptID: "attempt",
	}
	for _, test := range []struct {
		name  string
		input *rawStdin
	}{
		{name: "kind", input: &rawStdin{Kind: "other", Descriptor: 0, ByteLength: 1, ChannelID: "c", RunID: "r", ProfileID: "p", AttemptID: "a"}},
		{name: "descriptor", input: &rawStdin{Kind: rawStdinKind, Descriptor: 3, ByteLength: 1, ChannelID: "c", RunID: "r", ProfileID: "p", AttemptID: "a"}},
		{name: "empty", input: &rawStdin{Kind: rawStdinKind, Descriptor: 0, ByteLength: 0, ChannelID: "c", RunID: "r", ProfileID: "p", AttemptID: "a"}},
		{name: "scope", input: &rawStdin{Kind: rawStdinKind, Descriptor: 0, ByteLength: 1, ChannelID: "secret scope", RunID: "r", ProfileID: "p", AttemptID: "a"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := validateRawStdin(test.input); err == nil {
				t.Fatal("invalid raw stdin authority was accepted")
			}
		})
	}
	request := validStartRequest(t)
	request.Stdin = input
	if err := validateStartRequest(request); err != nil {
		t.Fatalf("validateStartRequest with raw stdin authority: %v", err)
	}
}

func TestTerminateRequestRequiresExactPrivateIdentity(t *testing.T) {
	t.Parallel()
	start := validStartRequest(t)
	valid := terminateRequest{
		SchemaVersion: protocolSchemaVersion,
		Type:          requestTypeTerminate,
		OperationID:   start.OperationID,
		Nonce:         start.Nonce,
		Reason:        terminateReasonParentRequest,
	}
	if err := validateTerminateRequest(valid, start); err != nil {
		t.Fatalf("valid terminate request: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*terminateRequest)
	}{
		{name: "schema", mutate: func(request *terminateRequest) { request.SchemaVersion++ }},
		{name: "type", mutate: func(request *terminateRequest) { request.Type = "other" }},
		{name: "operation", mutate: func(request *terminateRequest) { request.OperationID += "-other" }},
		{name: "nonce", mutate: func(request *terminateRequest) { request.Nonce = strings.Repeat("0", nonceEncodedBytes) }},
		{name: "reason", mutate: func(request *terminateRequest) { request.Reason = "other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := valid
			test.mutate(&request)
			if err := validateTerminateRequest(request, start); err == nil {
				t.Fatal("invalid terminate request was accepted")
			}
		})
	}
}

func TestStatusMatrixAndCreateNewPublication(t *testing.T) {
	t.Parallel()
	status := supervisorStatus{
		SchemaVersion:      protocolSchemaVersion,
		OperationID:        "test-operation",
		Nonce:              testNonce,
		SupervisionOutcome: statusOutcomeTreeEmpty,
		TerminationReason:  terminationReasonNatural,
		TimedOut:           false,
		ActiveProcessCount: 0,
		InputOutcome:       inputOutcomeNotRequested,
		Root:               &rootStatus{PID: 42, ExitCode: 259},
		SpawnFailure:       nil,
	}
	path := filepath.Join(t.TempDir(), "status.json")
	if err := publishStatusNew(path, status); err != nil {
		t.Fatalf("publishStatusNew: %v", err)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalJSON(status)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, canonical) || bytes.HasSuffix(encoded, []byte{'\n'}) {
		t.Fatalf("published status = %q, canonical = %q", encoded, canonical)
	}
	if err := publishStatusNew(path, status); err == nil {
		t.Fatal("second publication overwrote create-new status")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, encoded) {
		t.Fatalf("status changed after rejected overwrite: %v", err)
	}

	spawnFailure := "not found"
	validSpawnFailure := supervisorStatus{
		SchemaVersion:      protocolSchemaVersion,
		OperationID:        "test-operation",
		Nonce:              testNonce,
		SupervisionOutcome: statusOutcomeSpawnFailed,
		TerminationReason:  terminationReasonTargetSpawnFailed,
		ActiveProcessCount: 0,
		InputOutcome:       inputOutcomeNotStarted,
		SpawnFailure:       &spawnFailure,
	}
	if err := validateStatus(validSpawnFailure); err != nil {
		t.Fatalf("valid spawn failure: %v", err)
	}
	invalid := status
	invalid.Root = nil
	if err := validateStatus(invalid); err == nil {
		t.Fatal("tree-empty status without root was accepted")
	}
	invalid = status
	invalid.TimedOut = true
	if err := validateStatus(invalid); err == nil {
		t.Fatal("timedOut status without deadline reason was accepted")
	}
	invalid = status
	invalid.Root = &rootStatus{PID: 0, ExitCode: 0}
	if err := validateStatus(invalid); err == nil {
		t.Fatal("tree-empty status with zero root PID was accepted")
	}
	decomposedFailure := "e\u0301"
	invalidSpawnFailure := validSpawnFailure
	invalidSpawnFailure.SpawnFailure = &decomposedFailure
	if err := validateStatus(invalidSpawnFailure); err == nil {
		t.Fatal("spawn-failed status with non-NFC diagnostic was accepted")
	}
	oversizedFailure := strings.Repeat("x", maximumDiagnosticBytes+1)
	invalidSpawnFailure.SpawnFailure = &oversizedFailure
	if err := validateStatus(invalidSpawnFailure); err == nil {
		t.Fatal("spawn-failed status with oversized diagnostic was accepted")
	}
}

func TestStatusValidationRejectsEveryAmbiguousAuthorityShape(t *testing.T) {
	spawnFailure := "not found"
	treeEmpty := supervisorStatus{
		SchemaVersion:      protocolSchemaVersion,
		OperationID:        "test-operation",
		Nonce:              testNonce,
		SupervisionOutcome: statusOutcomeTreeEmpty,
		TerminationReason:  terminationReasonNatural,
		InputOutcome:       inputOutcomeNotRequested,
		Root:               &rootStatus{PID: 42},
	}
	spawnFailed := supervisorStatus{
		SchemaVersion:      protocolSchemaVersion,
		OperationID:        "test-operation",
		Nonce:              testNonce,
		SupervisionOutcome: statusOutcomeSpawnFailed,
		TerminationReason:  terminationReasonTargetSpawnFailed,
		InputOutcome:       inputOutcomeNotStarted,
		SpawnFailure:       &spawnFailure,
	}
	tests := []struct {
		name   string
		status supervisorStatus
		mutate func(*supervisorStatus)
	}{
		{name: "schema", status: treeEmpty, mutate: func(value *supervisorStatus) { value.SchemaVersion++ }},
		{name: "identity", status: treeEmpty, mutate: func(value *supervisorStatus) { value.OperationID = "" }},
		{name: "active process", status: treeEmpty, mutate: func(value *supervisorStatus) { value.ActiveProcessCount = 1 }},
		{name: "tree spawn failure", status: treeEmpty, mutate: func(value *supervisorStatus) { value.SpawnFailure = &spawnFailure }},
		{name: "tree spawn reason", status: treeEmpty, mutate: func(value *supervisorStatus) { value.TerminationReason = terminationReasonTargetSpawnFailed }},
		{name: "tree input", status: treeEmpty, mutate: func(value *supervisorStatus) { value.InputOutcome = inputOutcomeNotStarted }},
		{name: "spawn root", status: spawnFailed, mutate: func(value *supervisorStatus) { value.Root = &rootStatus{PID: 42} }},
		{name: "spawn diagnostic", status: spawnFailed, mutate: func(value *supervisorStatus) { empty := ""; value.SpawnFailure = &empty }},
		{name: "spawn facts", status: spawnFailed, mutate: func(value *supervisorStatus) { value.TimedOut = true }},
		{name: "spawn input", status: spawnFailed, mutate: func(value *supervisorStatus) { value.InputOutcome = inputOutcomeNotRequested }},
		{name: "outcome", status: treeEmpty, mutate: func(value *supervisorStatus) { value.SupervisionOutcome = "other" }},
		{name: "reason", status: treeEmpty, mutate: func(value *supervisorStatus) { value.TerminationReason = "other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := test.status
			test.mutate(&status)
			if err := validateStatus(status); err == nil {
				t.Fatal("ambiguous status authority was accepted")
			}
		})
	}
}

func TestBoundedDiagnosticPreservesUTF8Boundary(t *testing.T) {
	t.Parallel()
	message := strings.Repeat("a", maximumDiagnosticBytes-1) + "界tail"
	bounded := boundedDiagnostic(errors.New(message))
	if len(bounded) > maximumDiagnosticBytes || !utf8.ValidString(bounded) {
		t.Fatalf("bounded diagnostic length=%d valid=%v", len(bounded), utf8.ValidString(bounded))
	}
	if normalized := boundedDiagnostic(errors.New("e\u0301")); normalized != "é" {
		t.Fatalf("bounded diagnostic did not normalize to NFC: %q", normalized)
	}
	if fallback := boundedDiagnostic(errors.New("")); fallback == "" {
		t.Fatal("bounded diagnostic returned an empty fallback")
	}
}

func validStartRequest(t *testing.T) startRequest {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return startRequest{
		SchemaVersion:      protocolSchemaVersion,
		Type:               requestTypeStart,
		OperationID:        "test-operation",
		Nonce:              testNonce,
		Executable:         filepath.Clean(executable),
		Arguments:          []string{"<>&\u2028"},
		CWD:                filepath.Clean(t.TempDir()),
		Environment:        []environmentEntry{},
		DeadlineMS:         5_000,
		TerminationGraceMS: 2_000,
	}
}
