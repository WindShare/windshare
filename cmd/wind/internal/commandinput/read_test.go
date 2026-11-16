package commandinput

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestReadPreservesInputResults(t *testing.T) {
	for _, ctx := range []context.Context{context.Background(), t.Context()} {
		value, err := Read(ctx, func() (string, error) { return "partial", io.EOF })
		if value != "partial" || !errors.Is(err, io.EOF) {
			t.Fatalf("value=%q err=%v", value, err)
		}
	}
	line, err := ReadLine(t.Context(), bufio.NewReader(strings.NewReader("answer\nnext\n")))
	if line != "answer\n" || err != nil {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestReadAlreadyCancelledDoesNotConsumeInput(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	value, err := Read(ctx, func() (string, error) {
		called = true
		return "answer", nil
	})
	if value != "" || !errors.Is(err, context.Canceled) || called {
		t.Fatalf("value=%q err=%v called=%v", value, err, called)
	}
}

func TestReadCancellationRejectsLateInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		input, writer := io.Pipe()
		defer input.Close()
		defer writer.Close()
		done := make(chan error, 1)
		readFinished := make(chan struct{})
		go func() {
			value, err := Read(ctx, func() (string, error) {
				defer close(readFinished)
				return bufio.NewReader(input).ReadString('\n')
			})
			if value != "" {
				t.Errorf("cancelled input returned %q", value)
			}
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error=%v", err)
		}
		if _, err := io.WriteString(writer, "late answer\n"); err != nil {
			t.Fatal(err)
		}
		<-readFinished
		synctest.Wait()
	})
}

func TestReadCancellationWinsReadyResult(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	value, err := Read(ctx, func() (string, error) {
		cancel()
		return "confirmation", nil
	})
	if value != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read accepted value=%q err=%v", value, err)
	}
}

func TestReadDeadlineUnblocksInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		input, writer := io.Pipe()
		defer input.Close()
		defer writer.Close()
		_, err := ReadLine(ctx, bufio.NewReader(input))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline error=%v", err)
		}
	})
}
