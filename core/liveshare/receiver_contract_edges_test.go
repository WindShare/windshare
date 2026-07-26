package liveshare

import (
	"sync"
	"testing"
)

func TestPreparedReceiverZeroValueCloseProvidesIdempotentJoin(t *testing.T) {
	receiver := &PreparedReceiver{}
	receiver.BeginClose()

	var callers sync.WaitGroup
	for range 4 {
		callers.Add(3)
		go func() {
			defer callers.Done()
			receiver.BeginClose()
		}()
		go func() {
			defer callers.Done()
			receiver.Close()
		}()
		go func() {
			defer callers.Done()
			receiver.WaitClosed()
		}()
	}
	callers.Wait()

	receiver.mu.Lock()
	closed, closeDone := receiver.closed, receiver.closeDone
	factory, resources := receiver.factory, receiver.resources
	receiver.mu.Unlock()
	if !closed || closeDone == nil {
		t.Fatalf("zero-value receiver close state = closed %v, done %v", closed, closeDone)
	}
	select {
	case <-closeDone:
	default:
		t.Fatal("all close joins returned before the shared completion signal closed")
	}
	if factory != nil || resources != nil {
		t.Fatal("zero-value receiver close manufactured runtime ownership")
	}
}
