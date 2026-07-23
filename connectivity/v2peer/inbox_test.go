package v2peer

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestPeerAttemptCloseDrainsCancellationWaiterAndRejectsLatePush(t *testing.T) {
	attempt := &peerAttempt{events: make(chan attemptEvent, 2), done: make(chan struct{})}
	completed := make(chan struct{})
	attempt.push(attemptEvent{kind: attemptOperationCanceled, completed: completed})
	attempt.closeInbox()
	select {
	case <-completed:
	default:
		t.Fatal("drained attempt event retained its cancellation waiter")
	}
	attempt.push(attemptEvent{kind: attemptRemoteCandidate})
	if len(attempt.events) != 0 {
		t.Fatal("late attempt event was enqueued after close")
	}
}

func TestSenderHandlerCloseDrainsCancellationWaiterAndClosesInboxAtomically(t *testing.T) {
	handler := &senderHandler{events: make(chan handlerEvent, 8)}
	completed := make(chan error, 1)
	if err := handler.enqueueCancellation(context.Background(), handlerEvent{
		kind: handlerCancel, completed: completed,
	}); err != nil {
		t.Fatal(err)
	}
	handler.closeInbox()
	if err := <-completed; !errors.Is(err, context.Canceled) {
		t.Fatalf("drained handler cancellation error=%v", err)
	}
	if err := handler.enqueue(context.Background(), handlerEvent{kind: handlerOffer}); !errors.Is(err, context.Canceled) {
		t.Fatalf("late handler enqueue error=%v", err)
	}
}

func TestReceiverAttemptCloseAndConcurrentPushLeaveNoRetainedEvents(t *testing.T) {
	attempt := &ReceiverAttempt{events: make(chan receiverEvent, 256)}
	const producers = 64
	start := make(chan struct{})
	var wait sync.WaitGroup
	for range producers {
		wait.Go(func() {
			<-start
			attempt.push(receiverEvent{kind: receiverConnectionFailed, err: errors.New("test")})
		})
	}
	close(start)
	attempt.closeInbox()
	wait.Wait()
	if len(attempt.events) != 0 {
		t.Fatalf("receiver attempt retained %d events after close", len(attempt.events))
	}
	attempt.push(receiverEvent{kind: receiverChannelOpened})
	if len(attempt.events) != 0 {
		t.Fatal("receiver attempt accepted a post-close event")
	}
}
