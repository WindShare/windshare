package nativeprobe

import (
	"bytes"
	"errors"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const observation = 20 * time.Millisecond
const completionLimit = 2 * time.Second
const maximumIPv4UDPPayload = 65507

type completion struct {
	bytes uint32
	err   error
}

type pendingReceive struct {
	handle      windows.Handle
	event       windows.Handle
	overlapped  windows.Overlapped
	buffer      []byte
	wsaBuffer   windows.WSABuf
	received    uint32
	flags       uint32
	result      completion
	completed   chan struct{}
	release     chan struct{}
	done        chan struct{}
	releaseOnce sync.Once
}

func socket(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func beginReceive(t *testing.T, conn *net.UDPConn) *pendingReceive {
	t.Helper()
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	op := &pendingReceive{
		event: event, buffer: make([]byte, 2048),
		completed: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{}),
	}
	op.overlapped.HEvent = event | 1
	op.wsaBuffer = windows.WSABuf{Len: uint32(len(op.buffer)), Buf: &op.buffer[0]}
	ready := make(chan error, 1)
	go func() {
		var pin runtime.Pinner
		pin.Pin(&op.overlapped)
		pin.Pin(&op.wsaBuffer)
		pin.Pin(&op.buffer[0])
		pin.Pin(&op.received)
		pin.Pin(&op.flags)
		defer func() { pin.Unpin(); _ = windows.CloseHandle(event); close(op.done) }()
		entered := false
		err := raw.Control(func(fd uintptr) {
			entered = true
			op.handle = windows.Handle(fd)
			startErr := windows.WSARecv(op.handle, &op.wsaBuffer, 1, &op.received, &op.flags, &op.overlapped, nil)
			ready <- startErr
			if errors.Is(startErr, windows.ERROR_IO_PENDING) {
				startErr = windows.WSAGetOverlappedResult(op.handle, &op.overlapped, &op.received, true, &op.flags)
			}
			op.result = completion{op.received, startErr}
			close(op.completed)
			// Keep the descriptor protected even when completion races cancellation.
			<-op.release
		})
		if !entered {
			op.result = completion{err: err}
			ready <- err
			close(op.completed)
		}
	}()
	t.Cleanup(func() {
		select {
		case <-op.done:
			return
		default:
		}
		_ = windows.CancelIoEx(op.handle, &op.overlapped)
		op.finish(t)
	})
	select {
	case err := <-ready:
		if !errors.Is(err, windows.ERROR_IO_PENDING) {
			t.Fatalf("expected pending receive: %v", err)
		}
	case <-time.After(completionLimit):
		t.Fatal("receive did not start")
	}
	return op
}

func (op *pendingReceive) cancel(t *testing.T) {
	t.Helper()
	err := windows.CancelIoEx(op.handle, &op.overlapped)
	if err != nil && !errors.Is(err, windows.ERROR_NOT_FOUND) {
		t.Fatal(err)
	}
}
func (op *pendingReceive) finish(t *testing.T) completion {
	t.Helper()
	select {
	case <-op.completed:
	case <-time.After(completionLimit):
		t.Fatal("native completion did not arrive")
	}
	op.releaseOnce.Do(func() { close(op.release) })
	select {
	case <-op.done:
	case <-time.After(completionLimit):
		t.Fatal("descriptor protection did not end")
	}
	return op.result
}
func pending(t *testing.T, op *pendingReceive) {
	t.Helper()
	select {
	case <-op.completed:
		t.Fatalf("unrelated receive ended: %+v", op.result)
	case <-time.After(observation):
	}
}

func TestTargetedCancellationKeepsOtherNativeOperation(t *testing.T) {
	conn := socket(t)
	first, second := beginReceive(t, conn), beginReceive(t, conn)
	first.cancel(t)
	if got := first.finish(t); !errors.Is(got.err, windows.ERROR_OPERATION_ABORTED) {
		t.Fatalf("cancelled operation: %+v", got)
	}
	pending(t, second)
	message := []byte("surviving-operation")
	if _, err := conn.WriteToUDP(message, conn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	got := second.finish(t)
	if got.err != nil || !bytes.Equal(second.buffer[:got.bytes], message) {
		t.Fatalf("survivor: %+v", got)
	}
}

func TestGoDeadlineLeavesManualNativeOperationOwned(t *testing.T) {
	conn := socket(t)
	op := beginReceive(t, conn)
	ordinary := make(chan error, 1)
	go func() { _, _, err := conn.ReadFromUDP(make([]byte, 128)); ordinary <- err }()
	if err := conn.SetReadDeadline(time.Now().Add(observation)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ordinary:
		if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
			t.Fatalf("ordinary read: %v", err)
		}
	case <-time.After(completionLimit):
		t.Fatal("ordinary read did not honor deadline")
	}
	pending(t, op)
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	op.cancel(t)
	if got := op.finish(t); !errors.Is(got.err, windows.ERROR_OPERATION_ABORTED) {
		t.Fatalf("manual operation: %+v", got)
	}
}

func TestCloseWaitsForEveryProtectedNativeOperation(t *testing.T) {
	conn := socket(t)
	first, second := beginReceive(t, conn), beginReceive(t, conn)
	closed := make(chan error, 1)
	go func() { closed <- conn.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("close preceded cancellation: %v", err)
	case <-time.After(observation):
	}
	first.cancel(t)
	first.finish(t)
	select {
	case err := <-closed:
		t.Fatalf("close preceded second completion: %v", err)
	case <-time.After(observation):
	}
	second.cancel(t)
	second.finish(t)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(completionLimit):
		t.Fatal("close did not finish after both native completions")
	}
}

func TestCancellationMayRaceSuccessfulCompletion(t *testing.T) {
	conn := socket(t)
	op := beginReceive(t, conn)
	message := []byte("completion-race")
	sent := make(chan error, 1)
	go func() { _, err := conn.WriteToUDP(message, conn.LocalAddr().(*net.UDPAddr)); sent <- err }()
	op.cancel(t)
	got := op.finish(t)
	if got.err != nil && !errors.Is(got.err, windows.ERROR_OPERATION_ABORTED) {
		t.Fatal(got.err)
	}
	if got.err == nil && !bytes.Equal(op.buffer[:got.bytes], message) {
		t.Fatal("completion returned wrong bytes")
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
}

// An asynchronous error can leave a successful datagram after the failed one.
func TestNativeBatchTracksCompletionPerDatagram(t *testing.T) {
	receiver, sender := socket(t), socket(t)
	target := receiver.LocalAddr().(*net.UDPAddr)
	port := uint16(target.Port)
	var addressStorage windows.RawSockaddrAny
	address := (*windows.RawSockaddrInet4)(unsafe.Pointer(&addressStorage))
	*address = windows.RawSockaddrInet4{Family: windows.AF_INET, Port: port<<8 | port>>8, Addr: [4]byte{127, 0, 0, 1}}
	packets := [][]byte{[]byte("accepted-prefix"), make([]byte, maximumIPv4UDPPayload+1), []byte("successful-tail")}
	type sendOperation struct {
		event      windows.Handle
		overlapped windows.Overlapped
		buffer     windows.WSABuf
	}
	operations := make([]sendOperation, len(packets))
	var pin runtime.Pinner
	pin.Pin(&addressStorage)
	for index, packet := range packets {
		event, err := windows.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer windows.CloseHandle(event)
		op := &operations[index]
		op.event = event
		op.overlapped.HEvent = event | 1
		op.buffer = windows.WSABuf{Len: uint32(len(packet)), Buf: &packet[0]}
		pin.Pin(&op.overlapped)
		pin.Pin(&op.buffer)
		pin.Pin(&packet[0])
	}
	defer pin.Unpin()
	raw, err := sender.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	accepted, completed := 0, 0
	outcomes := make([]completion, len(packets))
	var submitErr, completeErr error
	controlErr := raw.Control(func(fd uintptr) {
		handle := windows.Handle(fd)
		for index := range operations {
			op := &operations[index]
			err := windows.WSASendTo(handle, &op.buffer, 1, nil, 0,
				&addressStorage, int32(unsafe.Sizeof(*address)), &op.overlapped, nil)
			if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
				submitErr = err
				break
			}
			accepted++
		}
		for index := 0; index < accepted; index++ {
			op := &operations[index]
			state, err := windows.WaitForSingleObject(op.event, uint32(completionLimit/time.Millisecond))
			if err != nil || state == uint32(windows.WAIT_TIMEOUT) {
				_ = windows.CancelIoEx(handle, &op.overlapped)
				completeErr = errors.Join(completeErr, err, errors.New("send completion exceeded probe limit"))
			}
			var count, flags uint32
			err = windows.WSAGetOverlappedResult(handle, &op.overlapped, &count, true, &flags)
			outcomes[index] = completion{count, err}
			if err == nil && count == uint32(len(packets[index])) {
				completed++
			}
		}
	})
	t.Logf("accepted=%d successful=%d submission_error=%v outcomes=%+v", accepted, completed, submitErr, outcomes)
	if controlErr != nil || completeErr != nil {
		t.Fatal(errors.Join(controlErr, completeErr))
	}
	if submitErr != nil || accepted != 3 || completed != 2 || outcomes[0].err != nil ||
		!errors.Is(outcomes[1].err, windows.WSAEMSGSIZE) || outcomes[2].err != nil {
		t.Fatalf("accepted=%d completed=%d submission=%v outcomes=%+v", accepted, completed, submitErr, outcomes)
	}
	// Both successful datagrams are already sent; replaying either would duplicate it.
	if err := receiver.SetReadDeadline(time.Now().Add(completionLimit)); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]int)
	for range 2 {
		buffer := make([]byte, 128)
		count, _, err := receiver.ReadFromUDP(buffer)
		if err != nil {
			t.Fatal(err)
		}
		seen[string(buffer[:count])]++
	}
	if seen[string(packets[0])] != 1 || seen[string(packets[2])] != 1 {
		t.Fatalf("datagram boundaries: %v", seen)
	}
}
