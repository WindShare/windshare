//go:build windows

package main

import (
	"bufio"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"sync"
	"time"
)

type controlResult struct {
	request terminateRequest
	err     error
}

func watchParentProcess(request startRequest) (<-chan controlResult, func(), error) {
	parentPID := os.Getppid()
	if parentPID < 1 || uint64(parentPID) > maxWindowsProcessID {
		return nil, nil, errors.New("parent process identity is invalid")
	}
	handle, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(parentPID),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("retain parent process authority: %w", err)
	}
	if actual, identityErr := windows.GetProcessId(handle); identityErr != nil || actual != uint32(parentPID) {
		_ = windows.CloseHandle(handle)
		if identityErr != nil {
			return nil, nil, fmt.Errorf("authenticate parent process authority: %w", identityErr)
		}
		return nil, nil, errors.New("parent process identity changed while opened")
	}
	results := make(chan controlResult, 1)
	go func() {
		outcome, waitErr := windows.WaitForSingleObject(handle, windows.INFINITE)
		if waitErr != nil || outcome != windows.WAIT_OBJECT_0 {
			results <- controlResult{err: errors.Join(
				waitErr,
				errors.New("parent process liveness wait lost authority"),
			)}
			return
		}
		// A signaled handle proves that the exact retained parent ended. Treating
		// that event as an authenticated parent request lets the sole Job owner
		// finish cleanup and publish tree-empty evidence after its client exits.
		results <- controlResult{request: terminateRequest{
			SchemaVersion: protocolSchemaVersion,
			Type:          requestTypeTerminate,
			OperationID:   request.OperationID,
			Nonce:         request.Nonce,
			Reason:        terminateReasonParentRequest,
		}}
	}()
	var closeOnce sync.Once
	return results, func() { closeOnce.Do(func() { _ = windows.CloseHandle(handle) }) }, nil
}

func watchTerminationControl(
	controlPath string,
	request startRequest,
) (<-chan controlResult, func()) {
	results := make(chan controlResult, 1)
	stop := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		poll := time.NewTicker(jobPollInterval)
		defer poll.Stop()
		for {
			control, present, err := readTerminationControlFile(controlPath, request)
			if err != nil {
				select {
				case results <- controlResult{err: err}:
				case <-stop:
				}
				return
			}
			if present {
				select {
				case results <- controlResult{request: control}:
				case <-stop:
				}
				return
			}
			select {
			case <-poll.C:
			case <-stop:
				return
			}
		}
	}()
	return results, func() { stopOnce.Do(func() { close(stop) }) }
}

func readTerminationControlFile(
	path string,
	request startRequest,
) (terminateRequest, bool, error) {
	pathBefore, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return terminateRequest{}, false, nil
	}
	if err != nil {
		return terminateRequest{}, false, fmt.Errorf("inspect termination control: %w", err)
	}
	if !pathBefore.Mode().IsRegular() || pathBefore.Mode()&os.ModeSymlink != 0 {
		return terminateRequest{}, false, errors.New("termination control must be a regular no-follow file")
	}
	file, err := os.Open(path)
	if err != nil {
		return terminateRequest{}, false, fmt.Errorf("open termination control: %w", err)
	}
	defer file.Close()
	openedBefore, err := file.Stat()
	if err != nil {
		return terminateRequest{}, false, fmt.Errorf("inspect opened termination control: %w", err)
	}
	if !os.SameFile(pathBefore, openedBefore) || pathBefore.Size() != openedBefore.Size() ||
		!pathBefore.ModTime().Equal(openedBefore.ModTime()) {
		return terminateRequest{}, false, errors.New("termination control changed while it was opened")
	}
	reader := bufio.NewReaderSize(file, controlReaderBufferBytes)
	control, err := readCanonicalFrame[terminateRequest](reader, "termination control")
	if err != nil {
		return terminateRequest{}, false, err
	}
	if trailing, trailingErr := reader.ReadByte(); !errors.Is(trailingErr, io.EOF) || trailing != 0 {
		return terminateRequest{}, false, errors.New("termination control contains trailing bytes")
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return terminateRequest{}, false, fmt.Errorf("reinspect opened termination control: %w", err)
	}
	pathAfter, err := os.Lstat(path)
	if err != nil {
		return terminateRequest{}, false, fmt.Errorf("reinspect termination control path: %w", err)
	}
	if !os.SameFile(openedBefore, openedAfter) || !os.SameFile(openedAfter, pathAfter) ||
		openedBefore.Size() != openedAfter.Size() || openedAfter.Size() != pathAfter.Size() ||
		!openedBefore.ModTime().Equal(openedAfter.ModTime()) ||
		!openedAfter.ModTime().Equal(pathAfter.ModTime()) {
		return terminateRequest{}, false, errors.New("termination control changed while it was read")
	}
	if err := validateTerminateRequest(control, request); err != nil {
		return terminateRequest{}, false, err
	}
	return control, true, nil
}

func mergeControlAuthorities(
	sources ...<-chan controlResult,
) (<-chan controlResult, func()) {
	results := make(chan controlResult, len(sources))
	stop := make(chan struct{})
	var stopOnce sync.Once
	for _, source := range sources {
		go func() {
			select {
			case result := <-source:
				select {
				case results <- result:
				case <-stop:
				}
			case <-stop:
			}
		}()
	}
	return results, func() { stopOnce.Do(func() { close(stop) }) }
}
