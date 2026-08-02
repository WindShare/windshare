//go:build windows

package windowsjob

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

const discardReaderJoinWait = time.Second

func openSuperviseEndpoints(options superviseOptions) (_ superviseEndpoints, resultErr error) {
	status, err := openConfiguredPipe(
		options.statusHandle,
		options.statusPipe,
		windows.GENERIC_WRITE|windows.SYNCHRONIZE,
		"settlement",
	)
	if err != nil {
		return superviseEndpoints{}, err
	}
	control, err := openConfiguredPipe(
		options.controlHandle,
		options.controlPipe,
		windows.GENERIC_READ|windows.SYNCHRONIZE,
		"control",
	)
	if err != nil {
		return superviseEndpoints{}, errors.Join(err, status.Close())
	}
	parent, err := openConfiguredPipe(
		options.parentHandle,
		options.parentPipe,
		windows.GENERIC_READ|windows.SYNCHRONIZE,
		"parent liveness",
	)
	if err != nil {
		return superviseEndpoints{}, errors.Join(err, status.Close(), control.Close())
	}
	startEvidence, err := openConfiguredPipe(
		options.startEvidenceHandle,
		options.startEvidencePipe,
		windows.GENERIC_WRITE|windows.SYNCHRONIZE,
		"start evidence",
	)
	if err != nil {
		return superviseEndpoints{}, errors.Join(err, parent.Close(), control.Close(), status.Close())
	}
	startDecision, err := openConfiguredPipe(
		options.startDecisionHandle,
		options.startDecisionPipe,
		windows.GENERIC_READ|windows.SYNCHRONIZE,
		"start decision",
	)
	if err != nil {
		return superviseEndpoints{}, errors.Join(
			err, startEvidence.Close(), parent.Close(), control.Close(), status.Close(),
		)
	}
	var input *os.File
	if options.inputHandle != 0 || options.inputPipe != "" {
		input, err = openConfiguredPipe(
			options.inputHandle,
			options.inputPipe,
			windows.GENERIC_READ|windows.SYNCHRONIZE,
			"raw input",
		)
		if err != nil {
			return superviseEndpoints{}, errors.Join(
				err,
				startDecision.Close(),
				startEvidence.Close(),
				parent.Close(),
				control.Close(),
				status.Close(),
			)
		}
	}
	var event *os.File
	var discardReader *os.File
	var discardDone chan struct{}
	if options.eventHandle != 0 || options.eventPipe != "" {
		event, err = openConfiguredPipe(
			options.eventHandle,
			options.eventPipe,
			windows.GENERIC_WRITE|windows.SYNCHRONIZE,
			"test event",
		)
	} else {
		discardReader, event, err = os.Pipe()
		if err == nil {
			discardDone = make(chan struct{})
			go func() {
				_, _ = io.Copy(io.Discard, discardReader)
				close(discardDone)
			}()
		}
	}
	if err != nil {
		return superviseEndpoints{}, errors.Join(
			err,
			closeOptionalFile(input),
			startDecision.Close(),
			startEvidence.Close(),
			parent.Close(),
			control.Close(),
			status.Close(),
		)
	}
	var closeOnce sync.Once
	var closeErr error
	closeEndpoints := func() error {
		closeOnce.Do(func() {
			closeErr = errors.Join(
				closeOptionalFile(event),
				closeOptionalFile(input),
				closeOptionalFile(startDecision),
				closeOptionalFile(startEvidence),
				closeOptionalFile(parent),
				closeOptionalFile(control),
				closeOptionalFile(status),
			)
			if discardReader != nil {
				// Closing the reader cancels any synchronous discard read before the
				// bounded join; endpoint teardown must never wait indefinitely on a
				// transport that no longer contributes lifecycle evidence.
				closeErr = errors.Join(closeErr, closeOptionalFile(discardReader))
				timer := time.NewTimer(discardReaderJoinWait)
				select {
				case <-discardDone:
				case <-timer.C:
					closeErr = errors.Join(closeErr, errors.New("discard event reader did not stop within its bounded join"))
				}
				timer.Stop()
			}
		})
		return closeErr
	}
	return superviseEndpoints{
		status:        status,
		control:       control,
		input:         input,
		event:         event,
		parent:        parent,
		startEvidence: startEvidence,
		startDecision: startDecision,
		close:         closeEndpoints,
	}, nil
}

func closeOptionalFile(file *os.File) error {
	if file == nil {
		return nil
	}
	err := file.Close()
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

func openConfiguredPipe(handleValue uintptr, path string, access uint32, label string) (*os.File, error) {
	if handleValue != 0 {
		return adoptInheritedPipe(handleValue, access, label)
	}
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("encode %s pipe path: %w", label, err)
	}
	handle, err := windows.CreateFile(
		pathPointer,
		access|windows.FILE_READ_ATTRIBUTES,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("connect %s pipe: %w", label, err)
	}
	return adoptValidatedPipe(handle, label)
}

func adoptInheritedPipe(handleValue uintptr, access uint32, label string) (*os.File, error) {
	original := windows.Handle(handleValue)
	if err := windows.SetHandleInformation(original, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		_ = windows.CloseHandle(original)
		return nil, fmt.Errorf("make inherited %s endpoint private: %w", label, err)
	}
	if err := validatePipeHandle(original, label); err != nil {
		_ = windows.CloseHandle(original)
		return nil, err
	}
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(), original, windows.CurrentProcess(), &duplicate,
		access, false, 0,
	); err != nil {
		_ = windows.CloseHandle(original)
		return nil, fmt.Errorf("authenticate %s endpoint access: %w", label, err)
	}
	_ = windows.CloseHandle(original)
	return adoptAuthenticatedPipe(duplicate, label)
}

// The original handle has already passed GetNamedPipeInfo and the duplicate
// operation has authenticated the requested direction. Repeating the metadata
// query after deliberately reducing access can fail because that query needs
// rights unrelated to the endpoint's one-way data contract.
func adoptAuthenticatedPipe(handle windows.Handle, label string) (*os.File, error) {
	fileType, err := windows.GetFileType(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("inspect authenticated %s endpoint type: %w", label, err)
	}
	if fileType != windows.FILE_TYPE_PIPE {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("authenticated %s endpoint must be a pipe", label)
	}
	if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("make %s endpoint private: %w", label, err)
	}
	file := os.NewFile(uintptr(handle), "windshare-"+label+"-pipe")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("adopt %s endpoint", label)
	}
	return file, nil
}

func adoptValidatedPipe(handle windows.Handle, label string) (*os.File, error) {
	if err := validatePipeHandle(handle, label); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("make %s endpoint private: %w", label, err)
	}
	file := os.NewFile(uintptr(handle), "windshare-"+label+"-pipe")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("adopt %s endpoint", label)
	}
	return file, nil
}

func validatePipeHandle(handle windows.Handle, label string) error {
	fileType, err := windows.GetFileType(handle)
	if err != nil {
		return fmt.Errorf("inspect %s endpoint type: %w", label, err)
	}
	if fileType != windows.FILE_TYPE_PIPE {
		return fmt.Errorf("%s endpoint must be a pipe", label)
	}
	if err := windows.GetNamedPipeInfo(handle, nil, nil, nil, nil); err != nil {
		return fmt.Errorf("inspect %s pipe: %w", label, err)
	}
	return nil
}
