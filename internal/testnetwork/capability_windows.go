//go:build windows

package testnetwork

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

const (
	launchAuthorizationPipeEnv = "WINDSHARE_D5_AUTHORIZATION_PIPE"
	guardConnectTimeout        = 10 * time.Second
	maximumAuthorizationBytes  = 1 << 20
)

type authorizedProgram struct {
	Path   string `json:"Path"`
	Bytes  int64  `json:"Bytes"`
	SHA256 string `json:"SHA256"`
}

type authorizationPayload struct {
	SchemaVersion int                 `json:"SchemaVersion"`
	RunID         string              `json:"RunID"`
	Programs      []authorizedProgram `json:"Programs"`
}

type processAuthorization struct {
	runID    string
	programs map[string]authorizedProgram
	guard    windows.Handle
}

var (
	authorizationOnce sync.Once
	authorization     processAuthorization
	authorizationErr  error
	trackedMu         sync.Mutex
	trackedProcesses  []*os.Process
	runnerGuardLost   bool
)

func windowsHarnessAuthorized() bool {
	return ensureWindowsHarnessAuthorization() == nil
}

func ensureWindowsHarnessAuthorization() error {
	authorizationOnce.Do(func() {
		authorization, authorizationErr = loadWindowsHarnessAuthorization()
	})
	return authorizationErr
}

func loadWindowsHarnessAuthorization() (processAuthorization, error) {
	pipeName := os.Getenv(launchAuthorizationPipeEnv)
	guard, err := connectAuthorizationPipe(pipeName)
	if err != nil {
		return processAuthorization{}, err
	}
	// Failure paths discard the guard best-effort: the authorization error is
	// the actionable failure, and a close error on a handle being thrown away
	// has no recovery.
	guardAdopted := false
	defer func() {
		if !guardAdopted {
			_ = windows.CloseHandle(guard)
		}
	}()
	payload, err := readParentAuthorization(guard)
	if err != nil {
		return processAuthorization{}, err
	}
	programs, err := validateParentAuthorization(payload)
	if err != nil {
		return processAuthorization{}, err
	}
	result := processAuthorization{runID: payload.RunID, programs: programs, guard: guard}
	if err := result.verifyExecutable(currentExecutable()); err != nil {
		return processAuthorization{}, err
	}
	guardAdopted = true
	go monitorRunnerGuard(guard)
	return result, nil
}

func connectAuthorizationPipe(name string) (windows.Handle, error) {
	if !strings.HasPrefix(name, "windshare-d5-auth-") || strings.ContainsAny(name, `\\/`) {
		return 0, errors.New("parent-owned Windows launch authorization is missing or invalid")
	}
	pipePath, err := windows.UTF16PtrFromString(`\\.\pipe\` + name)
	if err != nil {
		return 0, fmt.Errorf("encode parent-owned launch authorization path: %w", err)
	}
	deadline := time.Now().Add(guardConnectTimeout)
	for {
		handle, err := windows.CreateFile(
			pipePath,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if err == nil {
			return handle, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("connect parent-owned launch authorization: %w", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func readParentAuthorization(handle windows.Handle) (authorizationPayload, error) {
	lengthBytes := make([]byte, 4)
	if err := readPipeExact(handle, lengthBytes); err != nil {
		return authorizationPayload{}, fmt.Errorf("read parent authorization length: %w", err)
	}
	length := binary.LittleEndian.Uint32(lengthBytes)
	if length == 0 || length > maximumAuthorizationBytes {
		return authorizationPayload{}, errors.New("parent authorization payload length is invalid")
	}
	raw := make([]byte, length)
	if err := readPipeExact(handle, raw); err != nil {
		return authorizationPayload{}, fmt.Errorf("read parent authorization payload: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var payload authorizationPayload
	if err := decoder.Decode(&payload); err != nil {
		return authorizationPayload{}, fmt.Errorf("decode parent authorization payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return authorizationPayload{}, errors.New("parent authorization payload contains trailing JSON")
	}
	return payload, nil
}

func readPipeExact(handle windows.Handle, destination []byte) error {
	for offset := 0; offset < len(destination); {
		var read uint32
		err := windows.ReadFile(handle, destination[offset:], &read, nil)
		if err != nil {
			return err
		}
		if read == 0 {
			return io.ErrUnexpectedEOF
		}
		offset += int(read)
	}
	return nil
}

func validateParentAuthorization(payload authorizationPayload) (map[string]authorizedProgram, error) {
	if payload.SchemaVersion != 1 || payload.RunID == "" || len(payload.Programs) == 0 {
		return nil, errors.New("parent authorization payload has an invalid run identity")
	}
	programs := make(map[string]authorizedProgram, len(payload.Programs))
	for _, program := range payload.Programs {
		path, err := filepath.Abs(program.Path)
		if err != nil || program.Bytes <= 0 {
			return nil, errors.New("parent authorization payload has an invalid program")
		}
		digest, err := hex.DecodeString(program.SHA256)
		if err != nil || len(digest) != sha256.Size {
			return nil, errors.New("parent authorization payload has an invalid program digest")
		}
		key := strings.ToLower(filepath.Clean(path))
		if _, duplicate := programs[key]; duplicate {
			return nil, errors.New("parent authorization payload repeats a program")
		}
		program.Path = path
		program.SHA256 = strings.ToLower(program.SHA256)
		programs[key] = program
	}
	return programs, nil
}

func currentExecutable() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return ""
	}
	return path
}

func (a processAuthorization) verifyExecutable(path string) error {
	if path == "" {
		return errors.New("resolve parent-authorized Windows harness executable")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve parent-authorized Windows harness executable: %w", err)
	}
	record, ok := a.programs[strings.ToLower(filepath.Clean(absolute))]
	if !ok {
		return fmt.Errorf("executable is absent from the parent-owned launch set: %s", absolute)
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return fmt.Errorf("read parent-authorized Windows harness executable: %w", err)
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != record.Bytes || hex.EncodeToString(digest[:]) != record.SHA256 {
		return fmt.Errorf("executable differs from the parent-owned launch set: %s", absolute)
	}
	return nil
}

func verifyWindowsAuthorizedExecutable(path string) error {
	if err := ensureWindowsHarnessAuthorization(); err != nil {
		return err
	}
	return authorization.verifyExecutable(path)
}

func startWindowsGuardedProcess(cmd *exec.Cmd) error {
	if err := verifyWindowsAuthorizedExecutable(cmd.Path); err != nil {
		return err
	}
	trackedMu.Lock()
	defer trackedMu.Unlock()
	if runnerGuardLost {
		return errors.New("the parent-owned Windows runner guard is no longer alive")
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	trackedProcesses = append(trackedProcesses, cmd.Process)
	return nil
}

func monitorRunnerGuard(handle windows.Handle) {
	buffer := []byte{0}
	var read uint32
	_ = windows.ReadFile(handle, buffer, &read, nil)
	trackedMu.Lock()
	runnerGuardLost = true
	processes := append([]*os.Process(nil), trackedProcesses...)
	trackedMu.Unlock()
	for _, process := range processes {
		_ = process.Kill()
	}
	os.Exit(125)
}
