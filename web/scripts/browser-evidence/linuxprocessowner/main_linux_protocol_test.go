//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRunMainCommandContract(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		arguments []string
		message   string
	}{
		{name: "missing", message: "exactly one command"},
		{name: "multiple", arguments: []string{commandRun, commandSelfCheck}, message: "exactly one command"},
		{name: "unknown", arguments: []string{"unknown"}, message: "unknown linux process owner command"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := runMain(testCase.arguments)
			if err == nil || !strings.Contains(err.Error(), testCase.message) {
				t.Fatalf("runMain(%q) error = %v", testCase.arguments, err)
			}
		})
	}

	output, err := captureRunMainStdout(t, []string{commandSelfCheck})
	if err != nil {
		t.Fatal(err)
	}
	const expected = "{\"schemaVersion\":1,\"component\":\"browser-evidence-linux-process-owner\",\"outcome\":\"ready\"}\n"
	if output != expected {
		t.Fatalf("self-check output = %q", output)
	}
}

func TestReadRequestEnforcesCanonicalProtocol(t *testing.T) {
	valid := validProtocolRequest()
	encoded := marshalRequest(t, valid)
	decoded, err := readRequest(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, valid) {
		t.Fatalf("decoded request = %#v", decoded)
	}

	unknownField := func() []byte {
		var object map[string]any
		if err := json.Unmarshal(encoded, &object); err != nil {
			t.Fatal(err)
		}
		object["unexpected"] = true
		result, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}()
	unsupportedSchema := valid
	unsupportedSchema.SchemaVersion = "windshare.linux-process-owner-request/v0"
	invalidOperation := valid
	invalidOperation.OperationID = "invalid operation"
	invalidDeadline := valid
	invalidDeadline.DeadlineMS = 0
	invalidGrace := valid
	invalidGrace.TerminationGraceMS = maximumTerminationGraceMS + 1
	invalidExecutable := valid
	invalidExecutable.Command.Executable = "relative"

	for _, testCase := range []struct {
		name    string
		encoded []byte
		message string
	}{
		{name: "empty", message: "empty or oversized"},
		{name: "oversized", encoded: bytes.Repeat([]byte{'x'}, maximumRequestBytes+1), message: "empty or oversized"},
		{name: "malformed", encoded: []byte("{"), message: "unexpected EOF"},
		{name: "unknown field", encoded: unknownField, message: "unknown field"},
		{name: "trailing JSON", encoded: append(append([]byte{}, encoded...), []byte("{}")...), message: "trailing JSON"},
		{name: "noncanonical", encoded: append(append([]byte{}, encoded...), '\n'), message: "not canonical"},
		{name: "schema", encoded: marshalRequest(t, unsupportedSchema), message: "schema is unsupported"},
		{name: "operation", encoded: marshalRequest(t, invalidOperation), message: "operation ID is invalid"},
		{name: "deadline", encoded: marshalRequest(t, invalidDeadline), message: "deadlines are outside"},
		{name: "grace", encoded: marshalRequest(t, invalidGrace), message: "deadlines are outside"},
		{name: "command", encoded: marshalRequest(t, invalidExecutable), message: "executable must be absolute"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := readRequest(bytes.NewReader(testCase.encoded))
			if err == nil || !strings.Contains(err.Error(), testCase.message) {
				t.Fatalf("readRequest error = %v", err)
			}
		})
	}

	sentinel := errors.New("request source failed")
	_, err = readRequest(readerFunc(func([]byte) (int, error) { return 0, sentinel }))
	if !errors.Is(err, sentinel) {
		t.Fatalf("reader failure = %v", err)
	}
}

func TestValidateCommandRejectsAmbiguousAuthority(t *testing.T) {
	validDigest := strings.Repeat("a", sha256.Size*2)
	validLength := int64(12)
	validStdin := &stdinAuthority{
		Descriptor: childInputDescriptor,
		ByteLength: 1,
		ChannelID:  "channel",
		RunID:      "run",
		ProfileID:  "profile",
		AttemptID:  "attempt",
	}
	fullyScoped := validProtocolCommand()
	fullyScoped.ExecutableSHA256 = &validDigest
	fullyScoped.ExecutableByteLength = &validLength
	fullyScoped.Stdin = validStdin
	if err := validateCommand(fullyScoped); err != nil {
		t.Fatalf("valid fully-scoped command: %v", err)
	}

	for _, testCase := range []struct {
		name    string
		mutate  func(*commandRequest)
		message string
	}{
		{name: "relative executable", mutate: func(command *commandRequest) { command.Executable = "bin/true" }, message: "executable must be absolute"},
		{name: "noncanonical executable", mutate: func(command *commandRequest) { command.Executable = "/bin/../bin/true" }, message: "executable must be absolute"},
		{name: "relative cwd", mutate: func(command *commandRequest) { command.CWD = "tmp" }, message: "working directory must be absolute"},
		{name: "noncanonical cwd", mutate: func(command *commandRequest) { command.CWD = "/tmp/.." }, message: "working directory must be absolute"},
		{name: "argument NUL", mutate: func(command *commandRequest) { command.Arguments = []string{"bad\x00argument"} }, message: "argument contains NUL"},
		{name: "empty environment name", mutate: func(command *commandRequest) { command.Environment = map[string]string{"": "value"} }, message: "environment contains an invalid entry"},
		{name: "environment separator", mutate: func(command *commandRequest) { command.Environment = map[string]string{"BAD=NAME": "value"} }, message: "environment contains an invalid entry"},
		{name: "environment NUL", mutate: func(command *commandRequest) { command.Environment = map[string]string{"NAME": "bad\x00value"} }, message: "environment contains an invalid entry"},
		{name: "digest alphabet", mutate: func(command *commandRequest) {
			digest := strings.Repeat("A", sha256.Size*2)
			command.ExecutableSHA256 = &digest
		}, message: "digest is invalid"},
		{name: "digest without length", mutate: func(command *commandRequest) { command.ExecutableSHA256 = &validDigest }, message: "digest and byte length must appear together"},
		{name: "length without digest", mutate: func(command *commandRequest) { command.ExecutableByteLength = &validLength }, message: "digest and byte length must appear together"},
		{name: "zero length", mutate: func(command *commandRequest) {
			length := int64(0)
			command.ExecutableSHA256 = &validDigest
			command.ExecutableByteLength = &length
		}, message: "byte length is invalid"},
		{name: "oversized length", mutate: func(command *commandRequest) {
			length := int64(maximumExecutableBytes + 1)
			command.ExecutableSHA256 = &validDigest
			command.ExecutableByteLength = &length
		}, message: "byte length is invalid"},
		{name: "stdin descriptor", mutate: func(command *commandRequest) {
			authority := *validStdin
			authority.Descriptor++
			command.Stdin = &authority
		}, message: "stdin framing is invalid"},
		{name: "stdin length", mutate: func(command *commandRequest) {
			authority := *validStdin
			authority.ByteLength = 0
			command.Stdin = &authority
		}, message: "stdin framing is invalid"},
		{name: "stdin scope", mutate: func(command *commandRequest) {
			authority := *validStdin
			authority.AttemptID = "bad/scope"
			command.Stdin = &authority
		}, message: "stdin scope is invalid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			command := validProtocolCommand()
			testCase.mutate(&command)
			err := validateCommand(command)
			if err == nil || !strings.Contains(err.Error(), testCase.message) {
				t.Fatalf("validateCommand error = %v", err)
			}
		})
	}
}

func TestOwnerProtocolReportsInvalidRequest(t *testing.T) {
	status, _ := runOwnerProtocolHarness(t, []byte("{}"), nil, false, "")
	if status.OperationID != "unknown" || status.Launched || status.TreeEmpty ||
		status.ProcessEvidence.ErrorCode != "REQUEST_INVALID" ||
		status.OwnershipEvidence.FailureCode != "REQUEST_INVALID" {
		t.Fatalf("invalid request status = %#v", status)
	}
}

func TestOwnerInitializationRejectsMissingExecutable(t *testing.T) {
	request := validProtocolRequest()
	request.Command.Executable = filepath.Join(t.TempDir(), "missing-target")
	status, _ := runOwnerProtocolHarness(t, marshalRequest(t, request), nil, false, "")
	if status.Launched || status.TreeEmpty || status.TimedOut ||
		status.ProcessEvidence.ErrorCode != "EXECUTABLE_INVALID" ||
		status.OwnershipEvidence.CleanupOutcome != "failed" {
		t.Fatalf("missing executable status = %#v", status)
	}
}

func TestExecutableAuthorityAuthenticatesHeldRevision(t *testing.T) {
	path, content := writeExecutableFixture(t)
	metadata, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256(content)
	digest := hex.EncodeToString(digestBytes[:])
	byteLength := metadata.Size()

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := authenticateHeldExecutable(file, &byteLength, &digest); err != nil {
		t.Fatalf("authenticate valid target: %v", err)
	}
	wrongLength := byteLength + 1
	if err := authenticateHeldExecutable(file, &wrongLength, &digest); err == nil ||
		!strings.Contains(err.Error(), "byte length differs") {
		t.Fatalf("length mismatch error = %v", err)
	}
	wrongDigest := strings.Repeat("0", sha256.Size*2)
	if err := authenticateHeldExecutable(file, &byteLength, &wrongDigest); err == nil ||
		!strings.Contains(err.Error(), "differs from its authority digest") {
		t.Fatalf("digest mismatch error = %v", err)
	}

	nonExecutable := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(nonExecutable, content, 0o600); err != nil {
		t.Fatal(err)
	}
	nonExecutableFile, err := os.Open(nonExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if err := authenticateHeldExecutable(nonExecutableFile, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "not a bounded executable regular file") {
		t.Fatalf("non-executable error = %v", err)
	}
	nonExecutableFile.Close()
	if err := authenticateHeldExecutable(nonExecutableFile, nil, nil); err == nil {
		t.Fatal("closed target descriptor was accepted")
	}

	authority, err := holdExecutable(path, &byteLength, &digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.assertLive(); err != nil {
		t.Fatal(err)
	}
	mutatedContent := append(append([]byte{}, content...), '#')
	if err := os.WriteFile(path, mutatedContent, 0o700); err != nil {
		authority.close()
		t.Fatal(err)
	}
	if err := authority.assertLive(); err == nil || !strings.Contains(err.Error(), "changed while held") {
		authority.close()
		t.Fatalf("changed revision error = %v", err)
	}
	authority.close()

	if _, err := holdExecutable(filepath.Join(t.TempDir(), "missing"), nil, nil); err == nil {
		t.Fatal("missing executable was accepted")
	}
	if _, err := holdExecutable(nonExecutable, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "not an executable regular") {
		t.Fatalf("non-executable hold error = %v", err)
	}
	symlink := filepath.Join(t.TempDir(), "target-link")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := holdExecutable(symlink, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "no-follow") {
		t.Fatalf("symlink hold error = %v", err)
	}
}

func TestDigestHeldExecutableDetectsGrowth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing-target")
	if err := os.WriteFile(path, []byte("initial"), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	appendFile, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendFile.Write([]byte("growth")); err != nil {
		appendFile.Close()
		t.Fatal(err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := digestHeldExecutable(file, int64(len("initial"))); err == nil ||
		!strings.Contains(err.Error(), "grew beyond") {
		t.Fatalf("growth error = %v", err)
	}
}

func TestLateExecutableAuthorityIsClosed(t *testing.T) {
	path, _ := writeExecutableFixture(t)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	preflight := make(chan executablePreflight, 1)
	preflight <- executablePreflight{authority: &executableAuthority{file: file}}
	closeLateExecutable(preflight)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := file.Stat(); err != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("late executable authority remained open")
}

func TestStreamChildInputEnforcesExactFraming(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 32*1024+1)
	output, err := streamInputToPipe(bytes.NewReader(payload), &stdinAuthority{ByteLength: int64(len(payload))})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, payload) {
		t.Fatalf("streamed bytes = %d, want %d", len(output), len(payload))
	}

	output, err = streamInputToPipe(bytes.NewReader(nil), nil)
	if err != nil || len(output) != 0 {
		t.Fatalf("empty stream = %q, %v", output, err)
	}
	if _, err := streamInputToPipe(strings.NewReader("short"), &stdinAuthority{ByteLength: 6}); err == nil ||
		!strings.Contains(err.Error(), "read exact child stdin") {
		t.Fatalf("short source error = %v", err)
	}
	if output, err := streamInputToPipe(strings.NewReader("extra"), &stdinAuthority{ByteLength: 4}); err == nil || string(output) != "extr" || !strings.Contains(err.Error(), "beyond its declared length") {
		t.Fatalf("trailing source = %q, %v", output, err)
	}
	sentinel := errors.New("source failure")
	if _, err := streamInputToPipe(readerFunc(func([]byte) (int, error) { return 0, sentinel }), &stdinAuthority{ByteLength: 1}); !errors.Is(err, sentinel) {
		t.Fatalf("source failure = %v", err)
	}
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := streamChildInput(strings.NewReader("x"), full, &stdinAuthority{ByteLength: 1}); err == nil ||
		!strings.Contains(err.Error(), "write exact child stdin") {
		t.Fatalf("destination failure = %v", err)
	}
}

func TestControlAndExecGateFraming(t *testing.T) {
	sentinel := errors.New("control failed")
	for _, testCase := range []struct {
		name    string
		reader  io.Reader
		outcome string
	}{
		{name: "request", reader: bytes.NewReader([]byte{1}), outcome: "parent-request"},
		{name: "eof", reader: bytes.NewReader(nil), outcome: "parent-eof"},
		{name: "failure", reader: readerFunc(func([]byte) (int, error) { return 0, sentinel }), outcome: "control-failure"},
		{name: "closed", reader: readerFunc(func([]byte) (int, error) { return 0, nil }), outcome: "control-closed"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := make(chan string, 1)
			watchControl(testCase.reader, result)
			if outcome := <-result; outcome != testCase.outcome {
				t.Fatalf("control outcome = %q", outcome)
			}
		})
	}
	if err := readExecGateReady(bytes.NewReader([]byte{1})); err != nil {
		t.Fatal(err)
	}
	for _, encoded := range [][]byte{nil, {0}} {
		if err := readExecGateReady(bytes.NewReader(encoded)); err == nil {
			t.Fatalf("exec gate readiness %v was accepted", encoded)
		}
	}
}

func TestCleanupAuthorityRejectsUnprovenProcesses(t *testing.T) {
	authority := newInventoryAuthority(os.Getpid())
	defer authority.close()
	if _, err := retireOwnedTree(authority, make(chan terminalResult), nil, nil, 0, 0); err == nil ||
		!strings.Contains(err.Error(), "grace must be positive") {
		t.Fatalf("zero grace error = %v", err)
	}
	unknown := processIdentity{PID: 999999, StartTimeTicks: 1}
	if err := authority.signalInventory([]processIdentity{unknown}, 0); err == nil ||
		!strings.Contains(err.Error(), "lacks an authenticated pidfd") {
		t.Fatalf("untracked signal error = %v", err)
	}
	current, err := readStableProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	current.StartTimeTicks++
	if err := authority.track(current); err == nil || !strings.Contains(err.Error(), "was reused") {
		t.Fatalf("stale process identity error = %v", err)
	}
	if err := authority.track(processIdentity{PID: 1 << 30, StartTimeTicks: 1}); err == nil ||
		!strings.Contains(err.Error(), "open pidfd") {
		t.Fatalf("missing pid identity error = %v", err)
	}
	if err := authority.signalAll(0); err != nil {
		t.Fatalf("signal empty owned inventory: %v", err)
	}
}

func TestCanonicalEnvironmentAndDiagnosticBounds(t *testing.T) {
	environment := canonicalEnvironment(map[string]string{"ZETA": "last", "ALPHA": "first"})
	if !reflect.DeepEqual(environment, []string{"ALPHA=first", "ZETA=last"}) {
		t.Fatalf("canonical environment = %q", environment)
	}
	if message := boundedDiagnostic(nil); message != "unknown linux process owner failure" {
		t.Fatalf("nil diagnostic = %q", message)
	}
	if message := boundedDiagnostic(staticError("")); message != "unknown linux process owner failure" {
		t.Fatalf("empty diagnostic = %q", message)
	}
	long := boundedDiagnostic(errors.New(strings.Repeat("x", maximumDiagnosticBytes+20)))
	if len(long) != maximumDiagnosticBytes {
		t.Fatalf("bounded diagnostic length = %d", len(long))
	}
	invalidUTF8 := boundedDiagnostic(staticError(string([]byte{0xff, 'x'})))
	if !utf8.ValidString(invalidUTF8) {
		t.Fatalf("diagnostic is not valid UTF-8: %q", invalidUTF8)
	}

	for _, token := range []string{"token-1_OK", strings.Repeat("a", 256)} {
		if !portableToken(token) {
			t.Fatalf("portable token rejected: %q", token)
		}
	}
	for _, token := range []string{"", "bad/token", strings.Repeat("a", 257)} {
		if portableToken(token) {
			t.Fatalf("non-portable token accepted: %q", token)
		}
	}
	if !lowercaseSHA256(strings.Repeat("f", sha256.Size*2)) {
		t.Fatal("valid SHA-256 was rejected")
	}
	for _, digest := range []string{"short", strings.Repeat("F", sha256.Size*2), strings.Repeat("g", sha256.Size*2)} {
		if lowercaseSHA256(digest) {
			t.Fatalf("invalid SHA-256 accepted: %q", digest)
		}
	}
}

func TestFailureStatusAndWriterEvidence(t *testing.T) {
	cause := errors.New("initialization failed")
	status := failedStatus("operation", "FAILED", cause)
	if status.ProcessEvidence.ErrorMessage != cause.Error() ||
		status.OwnershipEvidence.FailureMessage != cause.Error() {
		t.Fatalf("failure status = %#v", status)
	}
	evidence := terminalEvidence(nil, cause)
	if evidence.Terminal != "spawn-failed" || evidence.ErrorCode != "WAIT_FAILED" ||
		evidence.ErrorMessage != cause.Error() {
		t.Fatalf("nil terminal evidence = %#v", evidence)
	}
	sentinel := errors.New("status writer failed")
	if err := writeStatus(writerFunc(func([]byte) (int, error) { return 0, sentinel }), status); !errors.Is(err, sentinel) {
		t.Fatalf("status writer error = %v", err)
	}
}

func validProtocolRequest() ownerRequest {
	return ownerRequest{
		SchemaVersion:      requestSchemaVersion,
		OperationID:        "protocol-test",
		Command:            validProtocolCommand(),
		DeadlineMS:         1_000,
		TerminationGraceMS: 100,
	}
}

func validProtocolCommand() commandRequest {
	return commandRequest{
		Executable: "/bin/true",
		Arguments:  []string{"--version"},
		CWD:        "/",
		Environment: map[string]string{
			"LANG": "C",
		},
	}
}

func marshalRequest(t *testing.T, request ownerRequest) []byte {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func captureRunMainStdout(t *testing.T, arguments []string) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	defer func() { os.Stdout = previous }()
	os.Stdout = writer
	runErr := runMain(arguments)
	closeErr := writer.Close()
	os.Stdout = previous
	output, readErr := io.ReadAll(reader)
	reader.Close()
	return string(output), errors.Join(runErr, closeErr, readErr)
}

func writeExecutableFixture(t *testing.T) (string, []byte) {
	t.Helper()
	content := []byte("#!/bin/sh\nexit 0\n")
	path := filepath.Join(t.TempDir(), "target.sh")
	if err := os.WriteFile(path, content, 0o700); err != nil {
		t.Fatal(err)
	}
	return path, content
}

func streamInputToPipe(source io.Reader, authority *stdinAuthority) ([]byte, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	type readResult struct {
		data []byte
		err  error
	}
	read := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(reader)
		reader.Close()
		read <- readResult{data: data, err: err}
	}()
	streamErr := streamChildInput(source, writer, authority)
	result := <-read
	return result.data, errors.Join(streamErr, result.err)
}

type readerFunc func([]byte) (int, error)

func (read readerFunc) Read(buffer []byte) (int, error) {
	return read(buffer)
}

type writerFunc func([]byte) (int, error)

func (write writerFunc) Write(buffer []byte) (int, error) {
	return write(buffer)
}

type staticError string

func (message staticError) Error() string {
	return string(message)
}
