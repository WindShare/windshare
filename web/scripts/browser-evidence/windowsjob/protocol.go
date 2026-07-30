package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	protocolSchemaVersion  = 2
	maximumFrameBytes      = 1 << 20
	maximumStdinBytes      = 1 << 20
	maximumOperationBytes  = 256
	maximumDiagnosticBytes = 512
	minimumDeadlineMS      = 1
	maximumDeadlineMS      = 86_400_000
	minimumGraceMS         = 1
	maximumGraceMS         = 60_000
	nonceEncodedBytes      = 64
)

const rawStdinKind = "anonymous-pipe"

const (
	requestTypeStart             = "start"
	requestTypeTerminate         = "terminate"
	terminateReasonParentRequest = "parent-request"
)

const (
	statusOutcomeTreeEmpty             = "tree-empty"
	statusOutcomeSpawnFailed           = "spawn-failed"
	terminationReasonNatural           = "natural"
	terminationReasonTargetSpawnFailed = "target-spawn-failed"
	terminationReasonDeadline          = "deadline"
	inputOutcomeNotStarted             = "not-started"
	inputOutcomeNotRequested           = "not-requested"
	inputOutcomeDelivered              = "delivered"
)

type environmentEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type rawStdin struct {
	Kind       string `json:"kind"`
	Descriptor int    `json:"descriptor"`
	ByteLength int64  `json:"byteLength"`
	ChannelID  string `json:"channelId"`
	RunID      string `json:"runId"`
	ProfileID  string `json:"profileId"`
	AttemptID  string `json:"attemptId"`
}

type startRequest struct {
	SchemaVersion      int                `json:"schemaVersion"`
	Type               string             `json:"type"`
	OperationID        string             `json:"operationId"`
	Nonce              string             `json:"nonce"`
	Executable         string             `json:"executable"`
	Arguments          []string           `json:"arguments"`
	CWD                string             `json:"cwd"`
	Environment        []environmentEntry `json:"environment"`
	DeadlineMS         int64              `json:"deadlineMs"`
	TerminationGraceMS int64              `json:"terminationGraceMs"`
	ExecutableSHA256   string             `json:"executableSha256,omitempty"`
	Stdin              *rawStdin          `json:"stdin,omitempty"`
}

type terminateRequest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Type          string `json:"type"`
	OperationID   string `json:"operationId"`
	Nonce         string `json:"nonce"`
	Reason        string `json:"reason"`
}

type rootStatus struct {
	PID      uint32 `json:"pid"`
	ExitCode uint32 `json:"exitCode"`
}

type supervisorStatus struct {
	SchemaVersion      int         `json:"schemaVersion"`
	OperationID        string      `json:"operationId"`
	Nonce              string      `json:"nonce"`
	SupervisionOutcome string      `json:"supervisionOutcome"`
	TerminationReason  string      `json:"terminationReason"`
	TimedOut           bool        `json:"timedOut"`
	ActiveProcessCount uint32      `json:"activeProcessCount"`
	InputOutcome       string      `json:"inputOutcome"`
	Root               *rootStatus `json:"root"`
	SpawnFailure       *string     `json:"spawnFailure"`
}

func readCanonicalFrame[T any](reader io.Reader, label string) (T, error) {
	var zero T
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return zero, err
	}
	length := binary.BigEndian.Uint32(header)
	if length == 0 || length > maximumFrameBytes {
		return zero, fmt.Errorf("%s frame length must be in [1, %d]", label, maximumFrameBytes)
	}
	encoded := make([]byte, int(length))
	if _, err := io.ReadFull(reader, encoded); err != nil {
		return zero, fmt.Errorf("read %s frame: %w", label, err)
	}
	var decoded T
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return zero, fmt.Errorf("decode %s frame: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return zero, fmt.Errorf("%s frame contains trailing JSON data", label)
	}
	canonical, err := canonicalJSON(decoded)
	if err != nil {
		return zero, fmt.Errorf("canonicalize %s frame: %w", label, err)
	}
	if !bytes.Equal(encoded, canonical) {
		return zero, fmt.Errorf("%s frame is not canonical JSON", label)
	}
	return decoded, nil
}

func writeCanonicalFrame(writer io.Writer, value any) error {
	encoded, err := canonicalJSON(value)
	if err != nil {
		return err
	}
	if len(encoded) == 0 || len(encoded) > maximumFrameBytes {
		return fmt.Errorf("encoded frame length must be in [1, %d]", maximumFrameBytes)
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(encoded)))
	if err := writeAll(writer, header); err != nil {
		return err
	}
	return writeAll(writer, encoded)
}

func canonicalJSON(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	encoded := output.Bytes()
	if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
		return nil, errors.New("JSON encoder omitted record terminator")
	}
	return jsonStringifyCompatible(bytes.Clone(encoded[:len(encoded)-1])), nil
}

func jsonStringifyCompatible(encoded []byte) []byte {
	result := make([]byte, 0, len(encoded))
	for index := 0; index < len(encoded); {
		if encoded[index] != '\\' {
			result = append(result, encoded[index])
			index++
			continue
		}
		runEnd := index
		for runEnd < len(encoded) && encoded[runEnd] == '\\' {
			runEnd++
		}
		runLength := runEnd - index
		if runLength%2 == 1 && runEnd+5 <= len(encoded) {
			escape := string(encoded[runEnd : runEnd+5])
			if escape == "u2028" || escape == "u2029" {
				result = append(result, encoded[index:runEnd-1]...)
				if escape == "u2028" {
					result = append(result, "\u2028"...)
				} else {
					result = append(result, "\u2029"...)
				}
				index = runEnd + 5
				continue
			}
		}
		result = append(result, encoded[index:runEnd]...)
		index = runEnd
	}
	return result
}

func writeAll(writer io.Writer, encoded []byte) error {
	for len(encoded) > 0 {
		written, err := writer.Write(encoded)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		encoded = encoded[written:]
	}
	return nil
}

func validateStartRequest(request startRequest) error {
	if request.SchemaVersion != protocolSchemaVersion || request.Type != requestTypeStart {
		return errors.New("start request schema or type is unsupported")
	}
	if err := validateIdentity(request.OperationID, request.Nonce); err != nil {
		return err
	}
	if !filepath.IsAbs(request.Executable) || filepath.Clean(request.Executable) != request.Executable {
		return errors.New("executable must be an absolute canonical path")
	}
	if strings.IndexByte(request.Executable, 0) >= 0 {
		return errors.New("executable contains NUL")
	}
	if request.ExecutableSHA256 != "" {
		decoded, err := hex.DecodeString(request.ExecutableSHA256)
		if err != nil || len(decoded) != sha256.Size ||
			strings.ToLower(request.ExecutableSHA256) != request.ExecutableSHA256 {
			return errors.New("executableSha256 must be lowercase 64-hex")
		}
	}
	if request.Arguments == nil {
		return errors.New("arguments must be an array")
	}
	if request.Environment == nil {
		return errors.New("environment must be an array")
	}
	if request.CWD != "" && (!filepath.IsAbs(request.CWD) || filepath.Clean(request.CWD) != request.CWD) {
		return errors.New("cwd must be empty or an absolute canonical path")
	}
	if strings.IndexByte(request.CWD, 0) >= 0 {
		return errors.New("cwd contains NUL")
	}
	for _, argument := range request.Arguments {
		if strings.IndexByte(argument, 0) >= 0 {
			return errors.New("argument contains NUL")
		}
	}
	if request.DeadlineMS < minimumDeadlineMS || request.DeadlineMS > maximumDeadlineMS {
		return fmt.Errorf("deadlineMs must be in [%d, %d]", minimumDeadlineMS, maximumDeadlineMS)
	}
	if request.TerminationGraceMS < minimumGraceMS || request.TerminationGraceMS > maximumGraceMS {
		return fmt.Errorf("terminationGraceMs must be in [%d, %d]", minimumGraceMS, maximumGraceMS)
	}
	if err := validateEnvironment(request.Environment); err != nil {
		return err
	}
	return validateRawStdin(request.Stdin)
}

func validateRawStdin(input *rawStdin) error {
	if input == nil {
		return nil
	}
	if input.Kind != rawStdinKind || input.Descriptor != 0 {
		return errors.New("stdin must name the supervisor anonymous standard-input pipe")
	}
	if input.ByteLength < 1 || input.ByteLength > maximumStdinBytes {
		return fmt.Errorf("stdin byte length must be in [1, %d]", maximumStdinBytes)
	}
	for _, scope := range []string{input.ChannelID, input.RunID, input.ProfileID, input.AttemptID} {
		if !portableToken(scope) {
			return errors.New("stdin authority scope is invalid")
		}
	}
	return nil
}

func validateTerminateRequest(request terminateRequest, start startRequest) error {
	if request.SchemaVersion != protocolSchemaVersion || request.Type != requestTypeTerminate {
		return errors.New("terminate request schema or type is unsupported")
	}
	if request.OperationID != start.OperationID || request.Nonce != start.Nonce {
		return errors.New("terminate request identity does not match start request")
	}
	if request.Reason != terminateReasonParentRequest {
		return errors.New("terminate request reason is unsupported")
	}
	return nil
}

func validateIdentity(operationID, nonce string) error {
	if len(operationID) == 0 || len(operationID) > maximumOperationBytes {
		return fmt.Errorf("operationId length must be in [1, %d]", maximumOperationBytes)
	}
	if strings.IndexByte(operationID, 0) >= 0 {
		return errors.New("operationId contains NUL")
	}
	if !utf8.ValidString(operationID) || !norm.NFC.IsNormalString(operationID) {
		return errors.New("operationId must be valid NFC text")
	}
	if len(nonce) != nonceEncodedBytes {
		return fmt.Errorf("nonce must contain exactly %d lowercase hexadecimal characters", nonceEncodedBytes)
	}
	decoded := make([]byte, nonceEncodedBytes/2)
	if _, err := hex.Decode(decoded, []byte(nonce)); err != nil || strings.ToLower(nonce) != nonce {
		return errors.New("nonce must be lowercase hexadecimal")
	}
	return nil
}

func portableToken(value string) bool {
	if len(value) < 1 || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') &&
			!(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') &&
			!strings.ContainsRune("._-", character) {
			return false
		}
	}
	return true
}

func validateEnvironment(environment []environmentEntry) error {
	for index, entry := range environment {
		if entry.Name == "" || strings.ContainsAny(entry.Name, "=\x00") {
			return errors.New("environment name must be non-empty and exclude '=' and NUL")
		}
		if strings.IndexByte(entry.Value, 0) >= 0 {
			return fmt.Errorf("environment value for %q contains NUL", entry.Name)
		}
		for previousIndex := range index {
			if strings.EqualFold(environment[previousIndex].Name, entry.Name) {
				return fmt.Errorf("environment contains case-insensitive duplicate %q", entry.Name)
			}
		}
		if index == 0 {
			continue
		}
		previous := environment[index-1].Name
		comparison := compareEnvironmentNames(previous, entry.Name)
		if comparison > 0 {
			return errors.New("environment entries must be sorted by ASCII-folded UTF-8 name")
		}
	}
	return nil
}

func compareEnvironmentNames(left, right string) int {
	leftFolded := asciiFold(left)
	rightFolded := asciiFold(right)
	if comparison := bytes.Compare(leftFolded, rightFolded); comparison != 0 {
		return comparison
	}
	return bytes.Compare([]byte(left), []byte(right))
}

func asciiFold(value string) []byte {
	result := []byte(value)
	for index, value := range result {
		if value >= 'A' && value <= 'Z' {
			result[index] = value + ('a' - 'A')
		}
	}
	return result
}

func validateStatus(status supervisorStatus) error {
	if status.SchemaVersion != protocolSchemaVersion {
		return errors.New("status schema is unsupported")
	}
	if err := validateIdentity(status.OperationID, status.Nonce); err != nil {
		return err
	}
	if status.ActiveProcessCount != 0 {
		return errors.New("status may only be published after the job is empty")
	}
	switch status.SupervisionOutcome {
	case statusOutcomeTreeEmpty:
		if status.Root == nil || status.SpawnFailure != nil {
			return errors.New("tree-empty status requires root and excludes spawnFailure")
		}
		if status.Root.PID == 0 {
			return errors.New("tree-empty status requires a nonzero root PID")
		}
		if status.TerminationReason == terminationReasonTargetSpawnFailed {
			return errors.New("tree-empty status excludes target-spawn-failed")
		}
		if status.InputOutcome != inputOutcomeNotRequested && status.InputOutcome != inputOutcomeDelivered {
			return errors.New("tree-empty status requires terminal input evidence")
		}
	case statusOutcomeSpawnFailed:
		if status.Root != nil || status.SpawnFailure == nil || *status.SpawnFailure == "" {
			return errors.New("spawn-failed status requires spawnFailure and excludes root")
		}
		if !utf8.ValidString(*status.SpawnFailure) || !norm.NFC.IsNormalString(*status.SpawnFailure) || len(*status.SpawnFailure) > maximumDiagnosticBytes {
			return fmt.Errorf("spawnFailure must be valid NFC text containing at most %d UTF-8 bytes", maximumDiagnosticBytes)
		}
		if status.TerminationReason != terminationReasonTargetSpawnFailed || status.TimedOut {
			return errors.New("spawn-failed status has inconsistent termination facts")
		}
		if status.InputOutcome != inputOutcomeNotStarted {
			return errors.New("spawn-failed status requires not-started input evidence")
		}
	default:
		return errors.New("status supervisionOutcome is unsupported")
	}
	if status.TimedOut != (status.TerminationReason == terminationReasonDeadline) {
		return errors.New("status timedOut does not match terminationReason")
	}
	switch status.TerminationReason {
	case terminationReasonNatural, terminationReasonTargetSpawnFailed, terminationReasonDeadline, terminateReasonParentRequest:
	default:
		return errors.New("status terminationReason is unsupported")
	}
	return nil
}

func publishStatusNew(path string, status supervisorStatus) error {
	if err := validateStatus(status); err != nil {
		return err
	}
	if err := ensureFreshStatusDestination(path); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	encoded, err := canonicalJSON(status)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".windowsjob-status-*.tmp")
	if err != nil {
		return fmt.Errorf("create staged status: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("protect staged status: %w", err)
	}
	if err := writeAll(temporary, encoded); err != nil {
		return fmt.Errorf("write staged status: %w", err)
	}
	// Seek-and-rewrite is avoided here because the status is the authority
	// record; publishing is permitted only after the exact bytes are durable.
	if _, err := temporary.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("verify staged status position: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("flush staged status: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close staged status: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return fmt.Errorf("commit create-new status: %w", err)
	}
	committed = true
	return nil
}

func ensureFreshStatusDestination(path string) error {
	return ensureFreshPrivateDestination(path, "status")
}

func ensureFreshPrivateDestination(path, label string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%s path must be absolute and canonical", label)
	}
	parent := filepath.Dir(path)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("inspect %s directory: %w", label, err)
	}
	if !parentInfo.IsDir() {
		return fmt.Errorf("%s parent is not a directory", label)
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s path already exists", label)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s path: %w", label, err)
	}
	return nil
}

func boundedDiagnostic(err error) string {
	if err == nil {
		return "unknown failure"
	}
	message := norm.NFC.String(strings.ToValidUTF8(err.Error(), "\ufffd"))
	if message == "" {
		return "unknown failure"
	}
	if len(message) <= maximumDiagnosticBytes {
		return message
	}
	boundary := maximumDiagnosticBytes
	for boundary > 0 && !utf8.ValidString(message[:boundary]) {
		boundary--
	}
	return message[:boundary]
}
