//go:build windows

package windowsjob

import (
	"bytes"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

type controlResult struct {
	reason string
	err    error
}

const authorityWatcherJoinWait = time.Second

func watchParentProcess(request supervisionRequest) (<-chan controlResult, func() error, error) {
	if request.ParentHandle == 0 {
		return nil, nil, errors.New("parent liveness pipe is unavailable")
	}
	handle := windows.Handle(request.ParentHandle)
	if err := validatePipeHandle(handle, "parent liveness"); err != nil {
		return nil, nil, err
	}
	results, closeAuthority := watchRetainedParent(handle)
	return results, closeAuthority, nil
}

func watchRetainedParent(handle windows.Handle) (<-chan controlResult, func() error) {
	results := make(chan controlResult, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(results)
		for {
			select {
			case <-stop:
				return
			default:
			}
			available, waitErr := peekPipe(handle)
			switch {
			case errors.Is(waitErr, windows.ERROR_BROKEN_PIPE),
				errors.Is(waitErr, windows.ERROR_PIPE_NOT_CONNECTED):
				select {
				case results <- controlResult{reason: ownerprotocol.TerminationParentLost}:
				case <-stop:
				}
				return
			case waitErr != nil:
				select {
				case results <- controlResult{err: fmt.Errorf("parent process liveness wait lost authority: %w", waitErr)}:
				case <-stop:
				}
				return
			case available != 0:
				select {
				case results <- controlResult{err: errors.New("parent liveness pipe carried unexpected data")}:
				case <-stop:
				}
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(jobPollInterval):
			}
		}
	}()
	var closeOnce sync.Once
	var closeErr error
	return results, func() error {
		closeOnce.Do(func() {
			close(stop)
			closeErr = joinAuthorityWatcher(done, "parent-liveness watcher")
		})
		return closeErr
	}
}

var peekNamedPipeProcedure = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

func peekPipe(handle windows.Handle) (uint32, error) {
	var available uint32
	succeeded, _, callErr := peekNamedPipeProcedure.Call(
		uintptr(handle), 0, 0, 0, uintptr(unsafe.Pointer(&available)), 0,
	)
	if succeeded == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
			return 0, errno
		}
		return 0, errors.New("PeekNamedPipe failed without an error code")
	}
	return available, nil
}

func watchTerminationControl(
	controlFile *os.File,
	request supervisionRequest,
) (<-chan controlResult, func() error) {
	results := make(chan controlResult, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	var stopOnce sync.Once
	var stopErr error
	go func() {
		defer close(done)
		defer close(results)
		control, canceled, err := readTerminationControl(controlFile, request, stop)
		if canceled {
			return
		}
		if err != nil {
			select {
			case results <- controlResult{err: err}:
			case <-stop:
			}
			return
		}
		select {
		case results <- controlResult{reason: control.Reason}:
		case <-stop:
		}
	}()
	return results, func() error {
		stopOnce.Do(func() {
			close(stop)
			stopErr = joinAuthorityWatcher(done, "termination-control watcher")
		})
		return stopErr
	}
}

func readTerminationControl(
	file *os.File,
	request supervisionRequest,
	stop <-chan struct{},
) (ownerprotocol.Control, bool, error) {
	if file == nil {
		return ownerprotocol.Control{}, false, errors.New("termination control endpoint is unavailable")
	}
	encoded := make([]byte, 0, controlReaderBufferBytes)
	poll := time.NewTicker(jobPollInterval)
	defer poll.Stop()
	for {
		select {
		case <-stop:
			return ownerprotocol.Control{}, true, nil
		default:
		}
		available, err := peekPipe(windows.Handle(file.Fd()))
		if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) {
			if len(encoded) == 0 {
				// The control stream is optional authority, while parent liveness is
				// mandatory. A bare EOF therefore retires only this source; treating it
				// as malformed would race parent EOF and make client death nondeterministic.
				return ownerprotocol.Control{}, true, nil
			}
			control, decodeErr := decodeTerminationControl(encoded, request)
			return control, false, decodeErr
		}
		if err != nil {
			return ownerprotocol.Control{}, false, fmt.Errorf("inspect termination control endpoint: %w", err)
		}
		if available > 0 {
			remaining := ownerprotocol.MaximumDocumentBytes + 5 - len(encoded)
			if remaining <= 0 {
				return ownerprotocol.Control{}, false, errors.New("termination control exceeds its bounded frame")
			}
			readSize := int(available)
			if readSize > remaining {
				readSize = remaining
			}
			chunk := make([]byte, readSize)
			count, readErr := file.Read(chunk)
			encoded = append(encoded, chunk[:count]...)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return ownerprotocol.Control{}, false, fmt.Errorf("read termination control endpoint: %w", readErr)
			}
			if count == 0 && readErr == nil {
				return ownerprotocol.Control{}, false, io.ErrNoProgress
			}
			continue
		}
		select {
		case <-stop:
			return ownerprotocol.Control{}, true, nil
		case <-poll.C:
		}
	}
}

func decodeTerminationControl(
	encoded []byte,
	request supervisionRequest,
) (ownerprotocol.Control, error) {
	reader := bytes.NewReader(encoded)
	control, err := ownerprotocol.ReadFrame[ownerprotocol.Control](reader)
	if err != nil {
		return ownerprotocol.Control{}, err
	}
	if err := ownerprotocol.ValidateControl(control, request.Identity); err != nil {
		return ownerprotocol.Control{}, err
	}
	if reader.Len() != 0 {
		return ownerprotocol.Control{}, errors.New("termination control contains trailing bytes")
	}
	return control, nil
}

func mergeControlAuthorities(
	sources ...<-chan controlResult,
) (<-chan controlResult, func() error) {
	results := make(chan controlResult, len(sources))
	stop := make(chan struct{})
	done := make(chan struct{})
	var stopOnce sync.Once
	var stopErr error
	var forwarders sync.WaitGroup
	forwarders.Add(len(sources))
	for _, source := range sources {
		go func() {
			defer forwarders.Done()
			select {
			case result, ok := <-source:
				if !ok {
					return
				}
				select {
				case results <- result:
				case <-stop:
				}
			case <-stop:
			}
		}()
	}
	go func() {
		forwarders.Wait()
		close(done)
	}()
	return results, func() error {
		stopOnce.Do(func() {
			close(stop)
			stopErr = joinAuthorityWatcher(done, "control-authority merger")
		})
		return stopErr
	}
}

func joinAuthorityWatcher(done <-chan struct{}, label string) error {
	timer := time.NewTimer(authorityWatcherJoinWait)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return fmt.Errorf("%s did not stop within its bounded join", label)
	}
}
