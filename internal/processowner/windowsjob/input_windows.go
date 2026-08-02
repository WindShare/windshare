//go:build windows

package windowsjob

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/windows"
)

const (
	targetInputBufferBytes      = 32 << 10
	maximumTargetInputAbortWait = time.Second
)

type targetInputDelivery struct {
	source      *os.File
	destination *os.File
	done        chan error
	thread      windows.Handle

	cancelOnce      sync.Once
	closeFilesOnce  sync.Once
	closeThreadOnce sync.Once
	closeFilesErr   error
}

// unstartedInputDrain owns the request-bound input capability when target
// creation fails. Draining the declared frame before publishing spawn failure
// lets clients finish their one-way pipe without mistaking an expected target
// outcome for a transport failure.
type unstartedInputDrain struct {
	source *os.File
	done   chan struct{}
	thread windows.Handle

	resultMu sync.Mutex
	result   error

	cancelOnce      sync.Once
	closeSourceOnce sync.Once
	closeThreadOnce sync.Once
	cancelErr       error
	closeSourceErr  error
}

type targetInputStart struct {
	thread windows.Handle
	err    error
}

func startUnstartedInputDrain(
	source *os.File,
	authority *ownerprotocol.Stdin,
) (*unstartedInputDrain, error) {
	if authority == nil {
		if source != nil {
			return nil, errors.New("input-free spawn failure received a raw-input capability")
		}
		return nil, nil
	}
	if source == nil {
		return nil, errors.New("spawn failure omitted its declared raw-input capability")
	}
	drain := &unstartedInputDrain{source: source, done: make(chan struct{})}
	ready := make(chan targetInputStart, 1)
	go drain.run(authority, ready)
	started := <-ready
	if started.err != nil {
		return nil, started.err
	}
	drain.thread = started.thread
	return drain, nil
}

func (drain *unstartedInputDrain) run(authority *ownerprotocol.Stdin, ready chan<- targetInputStart) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	thread, err := windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
	if err != nil {
		ready <- targetInputStart{err: fmt.Errorf("retain raw-input drain worker thread: %w", err)}
		drain.finish(errors.Join(err, drain.closeSource()))
		return
	}
	ready <- targetInputStart{thread: thread}
	drain.finish(errors.Join(
		streamExactTargetInput(drain.source, io.Discard, authority),
		drain.closeSource(),
	))
}

func (drain *unstartedInputDrain) finish(result error) {
	drain.resultMu.Lock()
	drain.result = result
	drain.resultMu.Unlock()
	close(drain.done)
}

func (drain *unstartedInputDrain) completed() <-chan struct{} {
	if drain == nil {
		return nil
	}
	return drain.done
}

func (drain *unstartedInputDrain) resultValue() error {
	if drain == nil {
		return nil
	}
	drain.resultMu.Lock()
	defer drain.resultMu.Unlock()
	return drain.result
}

func (drain *unstartedInputDrain) await(maximum time.Duration) error {
	if drain == nil {
		return nil
	}
	timer := time.NewTimer(maximum)
	defer timer.Stop()
	select {
	case <-drain.done:
		return drain.resultValue()
	case <-timer.C:
		return errors.New("raw-input drain did not complete within its lifecycle budget")
	}
}

func (drain *unstartedInputDrain) stopAndJoin(maximum time.Duration) error {
	if drain == nil {
		return nil
	}
	select {
	case <-drain.done:
		drain.closeThread()
		return drain.resultValue()
	default:
	}
	drain.cancelOnce.Do(func() {
		drain.cancelErr = errors.Join(
			cancelSynchronousThreadIO(drain.thread),
			cancelHandleIO(drain.source),
			drain.closeSource(),
		)
	})
	timer := time.NewTimer(maximum)
	defer timer.Stop()
	select {
	case <-drain.done:
		drain.closeThread()
		return errors.Join(drain.cancelErr, drain.resultValue())
	case <-timer.C:
		drain.closeThread()
		return errors.Join(drain.cancelErr, errors.New("raw-input drain did not stop within its bounded join"))
	}
}

func (drain *unstartedInputDrain) closeSource() error {
	drain.closeSourceOnce.Do(func() {
		drain.closeSourceErr = closeOptionalFile(drain.source)
	})
	return drain.closeSourceErr
}

func (drain *unstartedInputDrain) closeThread() {
	drain.closeThreadOnce.Do(func() {
		if drain.thread != 0 && drain.thread != windows.InvalidHandle {
			_ = windows.CloseHandle(drain.thread)
			drain.thread = 0
		}
	})
}

var cancelSynchronousIOProcedure = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")

func adoptLauncherInputWriter(
	launcherEvent launcherEvent,
	launcher *os.Process,
	authority *ownerprotocol.Stdin,
) (*os.File, error) {
	if authority == nil {
		if launcherEvent.InputHandle != 0 {
			return nil, errors.New("launcher transferred target stdin for an input-free request")
		}
		return nil, nil
	}
	if launcherEvent.InputHandle == 0 || uint64(uintptr(launcherEvent.InputHandle)) != launcherEvent.InputHandle {
		return nil, errors.New("launcher omitted a valid target stdin writer")
	}
	source := windows.Handle(uintptr(launcherEvent.InputHandle))
	if source == 0 || source == windows.InvalidHandle {
		return nil, errors.New("launcher-local target stdin writer is invalid")
	}
	var duplicate windows.Handle
	var duplicateErr error
	withHandleErr := launcher.WithHandle(func(launcherHandle uintptr) {
		duplicateErr = windows.DuplicateHandle(
			windows.Handle(launcherHandle),
			source,
			windows.CurrentProcess(),
			&duplicate,
			windows.GENERIC_WRITE|windows.SYNCHRONIZE,
			false,
			0,
		)
	})
	if withHandleErr != nil {
		return nil, fmt.Errorf("access launcher for target stdin transfer: %w", withHandleErr)
	}
	if duplicateErr != nil {
		return nil, fmt.Errorf("transfer target stdin writer from launcher: %w", duplicateErr)
	}
	return adoptAuthenticatedPipe(duplicate, "target input")
}

func startTargetInputDelivery(
	source *os.File,
	destination *os.File,
	authority *ownerprotocol.Stdin,
) (*targetInputDelivery, error) {
	if authority == nil {
		if source != nil || destination != nil {
			return nil, errors.New("input-free target received input capabilities")
		}
		return nil, nil
	}
	if source == nil || destination == nil {
		return nil, errors.New("declared target input requires source and destination capabilities")
	}
	delivery := &targetInputDelivery{
		source: source, destination: destination, done: make(chan error, 1),
	}
	ready := make(chan targetInputStart, 1)
	go delivery.run(authority, ready)
	started := <-ready
	if started.err != nil {
		return nil, started.err
	}
	delivery.thread = started.thread
	return delivery, nil
}

func (delivery *targetInputDelivery) run(authority *ownerprotocol.Stdin, ready chan<- targetInputStart) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	thread, err := windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
	if err != nil {
		ready <- targetInputStart{err: fmt.Errorf("retain target-input worker thread: %w", err)}
		delivery.done <- errors.Join(err, delivery.closeFiles())
		return
	}
	ready <- targetInputStart{thread: thread}
	streamErr := streamExactTargetInput(delivery.source, delivery.destination, authority)
	delivery.done <- errors.Join(streamErr, delivery.closeFiles())
}

func streamExactTargetInput(source io.Reader, destination io.Writer, authority *ownerprotocol.Stdin) error {
	if authority == nil {
		return errors.New("target input stream lacks declared byte authority")
	}
	buffer := make([]byte, targetInputBufferBytes)
	defer func() {
		for index := range buffer {
			buffer[index] = 0
		}
	}()
	remaining := authority.ByteLength
	var deliveryErr error
	for remaining > 0 {
		chunk := min(remaining, int64(len(buffer)))
		count, readErr := io.ReadFull(source, buffer[:chunk])
		if count > 0 && deliveryErr == nil {
			if err := writeAll(destination, buffer[:count]); err != nil {
				deliveryErr = fmt.Errorf("deliver exact target stdin: %w", err)
			}
		}
		for index := range count {
			buffer[index] = 0
		}
		remaining -= int64(count)
		if readErr != nil {
			return errors.Join(deliveryErr, fmt.Errorf("read declared target stdin bytes: %w", readErr))
		}
	}
	extra := make([]byte, 1)
	count, readErr := source.Read(extra)
	extra[0] = 0
	if count != 0 || !errors.Is(readErr, io.EOF) {
		deliveryErr = errors.Join(deliveryErr, errors.New("raw stdin pipe contains bytes beyond its declared length"))
	}
	return deliveryErr
}

func settleTargetInput(
	authority *ownerprotocol.Stdin,
	delivery *targetInputDelivery,
) (ownerprotocol.InputEvidence, error) {
	if authority == nil {
		if delivery != nil {
			failure := errors.New("input-free target retained an input delivery worker")
			return lostInputEvidence(failure), failure
		}
		return ownerprotocol.InputEvidence{Outcome: ownerprotocol.InputNotRequested}, nil
	}
	if delivery == nil {
		failure := errors.New("target input delivery evidence is unavailable")
		return lostInputEvidence(failure), failure
	}
	defer delivery.closeThread()
	select {
	case result := <-delivery.done:
		return completedInputEvidence(result), nil
	default:
	}
	cancelErr := delivery.cancel()
	timer := time.NewTimer(maximumTargetInputAbortWait)
	defer timer.Stop()
	select {
	case result := <-delivery.done:
		incomplete := errors.New("target tree settled before declared stdin delivery completed")
		return completedInputEvidence(errors.Join(incomplete, cancelErr, result)), nil
	case <-timer.C:
		failure := errors.Join(
			errors.New("target stdin delivery did not stop after its capabilities were revoked"),
			cancelErr,
		)
		return lostInputEvidence(failure), failure
	}
}

func completedInputEvidence(deliveryErr error) ownerprotocol.InputEvidence {
	if deliveryErr == nil {
		return ownerprotocol.InputEvidence{Outcome: ownerprotocol.InputDelivered}
	}
	return ownerprotocol.InputEvidence{
		Outcome: ownerprotocol.InputFailed, FailureCode: "TARGET_INPUT_DELIVERY_FAILED",
		FailureMessage: boundedDiagnostic(deliveryErr),
	}
}

func lostInputEvidence(cause error) ownerprotocol.InputEvidence {
	return ownerprotocol.InputEvidence{
		Outcome: ownerprotocol.InputEvidenceLost, FailureCode: "TARGET_INPUT_EVIDENCE_LOST",
		FailureMessage: boundedDiagnostic(cause),
	}
}

func (delivery *targetInputDelivery) cancel() error {
	if delivery == nil {
		return nil
	}
	var cancelErr error
	delivery.cancelOnce.Do(func() {
		cancelErr = errors.Join(
			cancelSynchronousThreadIO(delivery.thread),
			cancelHandleIO(delivery.source),
			cancelHandleIO(delivery.destination),
			delivery.closeFiles(),
		)
	})
	return cancelErr
}

func (delivery *targetInputDelivery) closeFiles() error {
	delivery.closeFilesOnce.Do(func() {
		delivery.closeFilesErr = errors.Join(
			closeOptionalFile(delivery.destination),
			closeOptionalFile(delivery.source),
		)
	})
	return delivery.closeFilesErr
}

func (delivery *targetInputDelivery) closeThread() {
	if delivery == nil {
		return
	}
	delivery.closeThreadOnce.Do(func() {
		if delivery.thread != 0 && delivery.thread != windows.InvalidHandle {
			_ = windows.CloseHandle(delivery.thread)
			delivery.thread = 0
		}
	})
}

func cancelHandleIO(file *os.File) error {
	if file == nil {
		return nil
	}
	err := windows.CancelIoEx(windows.Handle(file.Fd()), nil)
	if errors.Is(err, windows.ERROR_NOT_FOUND) {
		return nil
	}
	return err
}

func cancelSynchronousThreadIO(thread windows.Handle) error {
	if thread == 0 || thread == windows.InvalidHandle {
		return errors.New("target-input worker thread authority is unavailable")
	}
	result, _, callErr := cancelSynchronousIOProcedure.Call(uintptr(thread))
	if result != 0 || errors.Is(callErr, windows.ERROR_NOT_FOUND) {
		return nil
	}
	if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
		callErr = syscall.EINVAL
	}
	return fmt.Errorf("cancel target-input synchronous I/O: %w", callErr)
}
