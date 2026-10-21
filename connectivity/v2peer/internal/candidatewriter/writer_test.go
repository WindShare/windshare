package candidatewriter

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

const writerTestTimeout = 2 * time.Second

func await[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(writerTestTimeout):
		t.Fatal("candidate writer did not settle")
		var zero T
		return zero
	}
}

func TestWriterBoundsLifetimeAndPreservesOwnedFIFO(t *testing.T) {
	const maximum = 3
	entered, release := make(chan struct{}), make(chan struct{})
	sent := make(chan []byte, maximum)
	writer := New(context.Background(), maximum, func(ctx context.Context, body []byte) (bool, error) {
		if bytes.Equal(body, []byte{1}) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		sent <- body
		return true, nil
	})
	defer writer.Close()
	if err := writer.Enqueue([]byte{1}); err != nil {
		t.Fatal(err)
	}
	await(t, entered)
	for value := byte(2); value <= maximum; value++ {
		body := []byte{value}
		if err := writer.Enqueue(body); err != nil {
			t.Fatal(err)
		}
		body[0] = 99
	}
	if err := writer.Enqueue([]byte{4}); !errors.Is(err, ErrLimit) {
		t.Fatalf("unbounded candidate admission: %v", err)
	}
	close(release)
	for value := byte(1); value <= maximum; value++ {
		if body := await(t, sent); !bytes.Equal(body, []byte{value}) {
			t.Fatalf("candidate order or ownership changed: %v", body)
		}
		if result := await(t, writer.Results()); !result.Delivered || result.Err != nil {
			t.Fatalf("candidate delivery = %+v", result)
		}
	}
	if err := writer.Enqueue([]byte{4}); !errors.Is(err, ErrLimit) {
		t.Fatalf("draining the queue renewed the attempt quota: %v", err)
	}
}

func TestWriterCloseCancelsActiveSendAndDiscardsQueuedCandidates(t *testing.T) {
	entered := make(chan struct{}, 2)
	writer := New(context.Background(), 2, func(ctx context.Context, _ []byte) (bool, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return false, ctx.Err()
	})
	if err := writer.Enqueue([]byte{1}); err != nil {
		t.Fatal(err)
	}
	await(t, entered)
	if err := writer.Enqueue([]byte{2}); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	writer.Close()
	if len(entered) != 0 {
		t.Fatal("shutdown started another queued send")
	}
	if err := writer.Enqueue([]byte{3}); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed writer accepted a candidate: %v", err)
	}
	if result := await(t, writer.Results()); !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("in-flight outcome was erased: %+v", result)
	}
}

func TestWriterPreservesSendFailureWithoutSendingLaterCandidates(t *testing.T) {
	cause := errors.New("relay send failed")
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	writer := New(context.Background(), 2, func(context.Context, []byte) (bool, error) {
		entered <- struct{}{}
		<-release
		return false, cause
	})
	defer writer.Close()
	if err := writer.Enqueue([]byte{1}); err != nil {
		t.Fatal(err)
	}
	await(t, entered)
	if err := writer.Enqueue([]byte{2}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if result := await(t, writer.Results()); result.Delivered || !errors.Is(result.Err, cause) {
		t.Fatalf("failure was not preserved: %+v", result)
	}
	writer.Close()
	if len(entered) != 0 {
		t.Fatal("failed signaling started a later send")
	}
}

func TestWriterWithoutCandidatesClosesWithoutStartingIO(t *testing.T) {
	writer := New(context.Background(), 1, func(context.Context, []byte) (bool, error) {
		t.Error("idle candidate writer performed I/O")
		return false, nil
	})
	writer.Close()
}
