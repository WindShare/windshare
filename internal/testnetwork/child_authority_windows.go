//go:build windows

package testnetwork

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

const (
	childAuthorizationKind            = "child"
	childAuthorizationPipePrefix      = "windshare-d5-auth-child-v1-"
	childAuthorizationInvocationBytes = 16
	maximumChildAuthorizationBytes    = 4 << 10
	maximumChildOperationBytes        = 1 << 10
	childAuthorizationRetireTimeout   = 5 * time.Second
)

type childAuthorizationBinding struct {
	InvocationID    string
	OperationSHA256 string
}

type childAuthorizationRequest struct {
	SchemaVersion   int    `json:"SchemaVersion"`
	InvocationID    string `json:"InvocationID"`
	OperationSHA256 string `json:"OperationSHA256"`
}

type childAuthorizationServer struct {
	pipe               windows.Handle
	binding            childAuthorizationBinding
	expectedExecutable string
	parent             processAuthorization
	payload            authorizationPayload
	done               chan struct{}
	retired            atomic.Bool
	ioMu               sync.Mutex
	retireOnce         sync.Once
	retireErr          error
}

func newOSNetworkChildAuthority(executable, operationID string) (string, func() error, error) {
	if err := ensureWindowsHarnessAuthorization(); err != nil {
		return "", nil, fmt.Errorf("authenticate parent OS-network authority: %w", err)
	}
	return newOSNetworkChildAuthorityFor(authorization, executable, operationID)
}

func newOSNetworkChildAuthorityFor(
	parent processAuthorization,
	executable,
	operationID string,
) (string, func() error, error) {
	if strings.TrimSpace(operationID) == "" || len(operationID) > maximumChildOperationBytes {
		return "", nil, errors.New("child OS-network authority requires a bounded operation identity")
	}
	expectedExecutable, err := filepath.Abs(executable)
	if err != nil {
		return "", nil, fmt.Errorf("resolve child OS-network executable: %w", err)
	}
	if err := parent.verifyExecutable(expectedExecutable); err != nil {
		return "", nil, fmt.Errorf("bind child OS-network executable: %w", err)
	}
	binding, pipeName, err := newChildAuthorizationBinding(operationID)
	if err != nil {
		return "", nil, err
	}
	pipePath, err := windows.UTF16PtrFromString(`\\.\pipe\` + pipeName)
	if err != nil {
		return "", nil, fmt.Errorf("encode child authorization pipe: %w", err)
	}
	pipe, err := windows.CreateNamedPipe(
		pipePath,
		windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE|windows.FILE_FLAG_OVERLAPPED,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1,
		maximumAuthorizationBytes,
		maximumChildAuthorizationBytes,
		0,
		nil,
	)
	if err != nil {
		return "", nil, fmt.Errorf("create child authorization pipe: %w", err)
	}
	server := &childAuthorizationServer{
		pipe:               pipe,
		binding:            binding,
		expectedExecutable: expectedExecutable,
		parent:             parent,
		payload:            parent.childPayload(binding),
		done:               make(chan struct{}),
	}
	go server.serve()
	return pipeName, server.retire, nil
}

func newChildAuthorizationBinding(operationID string) (childAuthorizationBinding, string, error) {
	invocationBytes := make([]byte, childAuthorizationInvocationBytes)
	if _, err := rand.Read(invocationBytes); err != nil {
		return childAuthorizationBinding{}, "", fmt.Errorf("create child authorization invocation: %w", err)
	}
	operationDigest := sha256.Sum256([]byte(operationID))
	binding := childAuthorizationBinding{
		InvocationID:    hex.EncodeToString(invocationBytes),
		OperationSHA256: hex.EncodeToString(operationDigest[:]),
	}
	return binding, childAuthorizationPipePrefix + binding.InvocationID + "-" + binding.OperationSHA256, nil
}

func parseChildAuthorizationPipeName(name string) (childAuthorizationBinding, bool, error) {
	if !strings.HasPrefix(name, childAuthorizationPipePrefix) {
		return childAuthorizationBinding{}, false, nil
	}
	encoded := strings.TrimPrefix(name, childAuthorizationPipePrefix)
	expectedLength := hex.EncodedLen(childAuthorizationInvocationBytes) + 1 + hex.EncodedLen(sha256.Size)
	separator := hex.EncodedLen(childAuthorizationInvocationBytes)
	if len(encoded) != expectedLength || encoded[separator] != '-' {
		return childAuthorizationBinding{}, true, errors.New("child authorization pipe has an invalid binding")
	}
	binding := childAuthorizationBinding{
		InvocationID:    encoded[:separator],
		OperationSHA256: encoded[separator+1:],
	}
	if binding.InvocationID != strings.ToLower(binding.InvocationID) ||
		binding.OperationSHA256 != strings.ToLower(binding.OperationSHA256) {
		return childAuthorizationBinding{}, true, errors.New("child authorization pipe binding is not canonical")
	}
	invocation, invocationErr := hex.DecodeString(binding.InvocationID)
	operation, operationErr := hex.DecodeString(binding.OperationSHA256)
	if invocationErr != nil || operationErr != nil ||
		len(invocation) != childAuthorizationInvocationBytes || len(operation) != sha256.Size {
		return childAuthorizationBinding{}, true, errors.New("child authorization pipe binding is invalid")
	}
	return binding, true, nil
}

func writeChildAuthorizationRequest(handle windows.Handle, binding childAuthorizationBinding) error {
	request := childAuthorizationRequest{
		SchemaVersion:   1,
		InvocationID:    binding.InvocationID,
		OperationSHA256: binding.OperationSHA256,
	}
	if err := writePipeDocument(handle, request, maximumChildAuthorizationBytes); err != nil {
		return fmt.Errorf("write child authorization request: %w", err)
	}
	return nil
}

func (a processAuthorization) childPayload(binding childAuthorizationBinding) authorizationPayload {
	programs := make([]authorizedProgram, 0, len(a.programs))
	for _, program := range a.programs {
		programs = append(programs, program)
	}
	sort.Slice(programs, func(left, right int) bool {
		return strings.ToLower(programs[left].Path) < strings.ToLower(programs[right].Path)
	})
	return authorizationPayload{
		SchemaVersion:   1,
		RunID:           a.runID,
		Programs:        programs,
		AuthorityKind:   childAuthorizationKind,
		InvocationID:    binding.InvocationID,
		OperationSHA256: binding.OperationSHA256,
	}
}

func (s *childAuthorizationServer) serve() {
	// The serving goroutine owns all blocking pipe I/O. Retirement wakes that
	// I/O first and closes the handle only after this goroutine has stopped, so
	// Windows cannot strand cleanup inside CloseHandle.
	defer close(s.done)
	if err := s.connect(); err != nil {
		return
	}
	if s.retired.Load() {
		return
	}
	if err := s.authorizeConnectedChild(); err != nil {
		return
	}
	buffer := []byte{0}
	_ = s.readExact(s.pipe, buffer)
}

func (s *childAuthorizationServer) connect() error {
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return fmt.Errorf("create child authorization connect event: %w", err)
	}
	defer windows.CloseHandle(event) //nolint:errcheck
	overlapped := windows.Overlapped{HEvent: event}

	s.ioMu.Lock()
	if s.retired.Load() {
		s.ioMu.Unlock()
		return errors.New("child authorization authority is retired")
	}
	err = windows.ConnectNamedPipe(s.pipe, &overlapped)
	s.ioMu.Unlock()
	if err == nil || errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		return nil
	}
	if !errors.Is(err, windows.ERROR_IO_PENDING) {
		return fmt.Errorf("connect child authorization pipe: %w", err)
	}
	var transferred uint32
	if err := windows.GetOverlappedResult(s.pipe, &overlapped, &transferred, true); err != nil {
		return fmt.Errorf("complete child authorization connection: %w", err)
	}
	return nil
}

func (s *childAuthorizationServer) authorizeConnectedChild() error {
	if s.retired.Load() {
		return errors.New("child authorization authority is retired")
	}
	var childPID uint32
	if err := windows.GetNamedPipeClientProcessId(s.pipe, &childPID); err != nil {
		return fmt.Errorf("resolve child authorization PID: %w", err)
	}
	if childPID == 0 {
		return errors.New("child authorization PID is zero")
	}
	if err := verifyChildExecutable(childPID, s.expectedExecutable, s.parent); err != nil {
		return err
	}
	var request childAuthorizationRequest
	if err := readPipeDocumentWith(s.pipe, &request, maximumChildAuthorizationBytes, s.readExact); err != nil {
		return fmt.Errorf("read child authorization request: %w", err)
	}
	if request.SchemaVersion != 1 || request.InvocationID != s.binding.InvocationID ||
		request.OperationSHA256 != s.binding.OperationSHA256 {
		return errors.New("child authorization request does not match its operation binding")
	}
	if s.retired.Load() {
		return errors.New("child authorization authority retired before issuance")
	}
	if err := writePipeDocumentWith(s.pipe, s.payload, maximumAuthorizationBytes, s.writeExact); err != nil {
		return fmt.Errorf("write child authorization payload: %w", err)
	}
	return nil
}

func verifyChildExecutable(
	childPID uint32,
	expectedExecutable string,
	parent processAuthorization,
) error {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, childPID)
	if err != nil {
		return fmt.Errorf("open child authorization process: %w", err)
	}
	defer windows.CloseHandle(process) //nolint:errcheck
	pathBuffer := make([]uint16, windows.MAX_LONG_PATH)
	pathLength := uint32(len(pathBuffer))
	if err := windows.QueryFullProcessImageName(process, 0, &pathBuffer[0], &pathLength); err != nil {
		return fmt.Errorf("resolve child authorization executable: %w", err)
	}
	actualExecutable := windows.UTF16ToString(pathBuffer[:pathLength])
	expectedInfo, err := os.Stat(expectedExecutable)
	if err != nil {
		return fmt.Errorf("inspect expected child executable: %w", err)
	}
	actualInfo, err := os.Stat(actualExecutable)
	if err != nil {
		return fmt.Errorf("inspect connected child executable: %w", err)
	}
	if !os.SameFile(expectedInfo, actualInfo) {
		return errors.New("connected child executable differs from the delegated executable")
	}
	if err := parent.verifyExecutable(actualExecutable); err != nil {
		return fmt.Errorf("verify connected child executable: %w", err)
	}
	return nil
}

func readPipeDocumentWith(
	handle windows.Handle,
	destination any,
	maximumBytes uint32,
	readExact func(windows.Handle, []byte) error,
) error {
	lengthBytes := make([]byte, 4)
	if err := readExact(handle, lengthBytes); err != nil {
		return err
	}
	length := binary.LittleEndian.Uint32(lengthBytes)
	if length == 0 || length > maximumBytes {
		return errors.New("pipe document length is invalid")
	}
	raw := make([]byte, length)
	if err := readExact(handle, raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("pipe document contains trailing JSON")
	}
	return nil
}

func writePipeDocument(handle windows.Handle, value any, maximumBytes uint32) error {
	return writePipeDocumentWith(handle, value, maximumBytes, writePipeExact)
}

func writePipeDocumentWith(
	handle windows.Handle,
	value any,
	maximumBytes uint32,
	writeExact func(windows.Handle, []byte) error,
) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) == 0 || uint64(len(raw)) > uint64(maximumBytes) {
		return errors.New("pipe document exceeds its authority")
	}
	lengthBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(lengthBytes, uint32(len(raw)))
	if err := writeExact(handle, lengthBytes); err != nil {
		return err
	}
	return writeExact(handle, raw)
}

func writePipeExact(handle windows.Handle, source []byte) error {
	for offset := 0; offset < len(source); {
		var written uint32
		if err := windows.WriteFile(handle, source[offset:], &written, nil); err != nil {
			return err
		}
		if written == 0 {
			return io.ErrUnexpectedEOF
		}
		offset += int(written)
	}
	return nil
}

func (s *childAuthorizationServer) readExact(_ windows.Handle, destination []byte) error {
	for offset := 0; offset < len(destination); {
		read, err := s.read(destination[offset:])
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

func (s *childAuthorizationServer) read(destination []byte) (uint32, error) {
	return s.runOverlappedPipeIO(destination, windows.ReadFile)
}

type overlappedPipeIO func(
	windows.Handle,
	[]byte,
	*uint32,
	*windows.Overlapped,
) error

func (s *childAuthorizationServer) runOverlappedPipeIO(
	buffer []byte,
	issue overlappedPipeIO,
) (uint32, error) {
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(event) //nolint:errcheck
	overlapped := windows.Overlapped{HEvent: event}
	var transferred uint32

	// Retirement must not cross the checked issuance boundary, but the wait must
	// remain outside the lock so retirement can cancel a pending operation.
	s.ioMu.Lock()
	if s.retired.Load() {
		s.ioMu.Unlock()
		return 0, errors.New("child authorization authority is retired")
	}
	err = issue(s.pipe, buffer, &transferred, &overlapped)
	s.ioMu.Unlock()
	if err == nil {
		return transferred, nil
	}
	if !errors.Is(err, windows.ERROR_IO_PENDING) {
		return 0, err
	}
	if err := windows.GetOverlappedResult(s.pipe, &overlapped, &transferred, true); err != nil {
		return 0, err
	}
	return transferred, nil
}

func (s *childAuthorizationServer) writeExact(_ windows.Handle, source []byte) error {
	for offset := 0; offset < len(source); {
		written, err := s.write(source[offset:])
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrUnexpectedEOF
		}
		offset += int(written)
	}
	return nil
}

func (s *childAuthorizationServer) write(source []byte) (uint32, error) {
	return s.runOverlappedPipeIO(source, windows.WriteFile)
}

func (s *childAuthorizationServer) retire() error {
	s.retireOnce.Do(func() {
		s.retired.Store(true)
		select {
		case <-s.done:
		default:
			s.retireErr = s.interruptAndWait()
		}
		if s.retireErr == nil {
			s.retireErr = windows.CloseHandle(s.pipe)
		}
	})
	return s.retireErr
}

func (s *childAuthorizationServer) interruptAndWait() error {
	deadline := time.NewTimer(childAuthorizationRetireTimeout)
	defer deadline.Stop()

	// I/O issuance and cancellation share ioMu, which closes the otherwise
	// possible race where retirement misses an operation just before it starts.
	s.ioMu.Lock()
	cancelErr := windows.CancelIoEx(s.pipe, nil)
	if errors.Is(cancelErr, windows.ERROR_NOT_FOUND) {
		cancelErr = nil
	}
	disconnectErr := windows.DisconnectNamedPipe(s.pipe)
	if errors.Is(disconnectErr, windows.ERROR_PIPE_NOT_CONNECTED) {
		disconnectErr = nil
	}
	s.ioMu.Unlock()
	if err := errors.Join(cancelErr, disconnectErr); err != nil {
		return fmt.Errorf("interrupt child authorization pipe: %w", err)
	}
	return s.waitForRetiredServer(deadline.C)
}

func (s *childAuthorizationServer) waitForRetiredServer(deadline <-chan time.Time) error {
	select {
	case <-s.done:
		return nil
	case <-deadline:
		return errors.New("retire child authorization pipe: server did not stop")
	}
}
