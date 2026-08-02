package protocol

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/internal/testrun"
)

func TestRequestRoundTripUsesOneCanonicalContract(t *testing.T) {
	t.Parallel()
	request := validRequest(t)
	var frame bytes.Buffer
	if err := WriteFrame(&frame, request); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadFrame[Request](&frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRequest(decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Identity != request.Identity || decoded.Command.Executable != request.Command.Executable {
		t.Fatalf("decoded request = %#v, want identity %#v and executable %q", decoded, request.Identity, request.Command.Executable)
	}
}

func TestLineDocumentRoundTripRequiresOneLF(t *testing.T) {
	t.Parallel()
	request := validRequest(t)
	var line bytes.Buffer
	if err := WriteLineDocument(&line, request); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadLineDocument[Request](&line)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Identity != request.Identity {
		t.Fatalf("decoded identity = %#v, want %#v", decoded.Identity, request.Identity)
	}
	encoded, err := EncodeCanonical(request)
	if err != nil {
		t.Fatal(err)
	}
	for name, suffix := range map[string]string{
		"missing terminator": "",
		"extra terminator":   "\n\n",
		"CRLF terminator":    "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadLineDocument[Request](bytes.NewReader(append(bytes.Clone(encoded), suffix...))); err == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}

func TestRequestRejectsAuthorizationAndHashFields(t *testing.T) {
	t.Parallel()
	request := validRequest(t)
	encoded, err := EncodeCanonical(request)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	document["executable_sha256"] = "00"
	withHash, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCanonical[Request](withHash); err == nil {
		t.Fatal("request accepted an executable hash field")
	}
}

func TestCompletedCleanupRequiresTreeEmpty(t *testing.T) {
	t.Parallel()
	settlement := validSettlement(t)
	settlement.TreeState = TreeUnknown
	if err := ValidateSettlement(settlement); err == nil {
		t.Fatal("completed cleanup accepted a tree without emptiness proof")
	}
	settlement.TreeState = TreeProvenEmpty
	if err := ValidateSettlement(settlement); err != nil {
		t.Fatal(err)
	}
}

func TestEnvironmentMustBeCanonicalAndCrossPlatformSafe(t *testing.T) {
	t.Parallel()
	request := validRequest(t)
	request.Command.Environment = []EnvironmentEntry{{Name: "Path", Value: "one"}, {Name: "PATH", Value: "two"}}
	if err := ValidateRequest(request); err == nil {
		t.Fatal("request accepted case-insensitive duplicate environment names")
	}
	request.Command.Environment = []EnvironmentEntry{{Name: "ZED", Value: "one"}, {Name: "alpha", Value: "two"}}
	if err := ValidateRequest(request); err == nil {
		t.Fatal("request accepted noncanonical environment order")
	}
}

func TestEventRequiresFullTraceIdentity(t *testing.T) {
	t.Parallel()
	event := Event{
		SchemaVersion: EventSchemaVersion,
		Identity:      validRequest(t).Identity,
		Component:     "relay", Milestone: "ready", Outcome: "succeeded",
		Payload: json.RawMessage(`{"address":"127.0.0.1:54321"}`),
	}
	if err := ValidateEvent(event); err != nil {
		t.Fatal(err)
	}
	event.Scenario = ""
	if err := ValidateEvent(event); err == nil {
		t.Fatal("event accepted a missing scenario")
	}
}

func TestTestrunJSONLineMatchesProcessOwnerCanonicalEncoding(t *testing.T) {
	t.Parallel()
	event := Event{
		SchemaVersion: EventSchemaVersion,
		Identity:      Identity{RunID: "run-1", OperationID: "operation-1", Scenario: "scenario"},
		Component:     "fixture", Milestone: "listener_ready", Outcome: "succeeded",
		Payload: json.RawMessage(`{"text":"line\u2028<&"}`),
	}
	var traceLine bytes.Buffer
	sink, err := testrun.NewJSONLineSink(&traceLine)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteEvent(event); err != nil {
		t.Fatal(err)
	}
	var ownerLine bytes.Buffer
	if err := WriteLineDocument(&ownerLine, event); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(traceLine.Bytes(), ownerLine.Bytes()) {
		t.Fatalf("testrun JSONL = %q, process-owner JSONL = %q", traceLine.Bytes(), ownerLine.Bytes())
	}
}

func validRequest(t *testing.T) Request {
	t.Helper()
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "fixture")
	return NewRequest(
		Identity{RunID: "run-1", OperationID: "operation-1", Scenario: "process-tree"},
		Command{
			Executable: executable, Arguments: []string{"child"}, WorkingDirectory: root,
			Environment: []EnvironmentEntry{{Name: "ALPHA", Value: "one"}, {Name: "beta", Value: "two"}},
		},
		1_000,
		500,
	)
}
