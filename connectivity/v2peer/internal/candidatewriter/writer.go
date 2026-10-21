// Package candidatewriter isolates ordered ICE signaling I/O from peer lifecycle
// dispatch. An established transport must not wait for spare paths to be advertised.
package candidatewriter

import (
	"bytes"
	"context"
	"errors"
)

var ErrLimit = errors.New("candidate writer: attempt candidate limit exceeded")

type Result struct {
	Delivered bool
	Err       error
}

type Send func(context.Context, []byte) (bool, error)

// Writer has one owning event loop. Enqueue, Stop and Close are called by that
// owner; only the worker invokes Send. The lifetime quota bounds both queued
// bodies and undrained results, even when signaling never makes progress.
type Writer struct {
	ctx     context.Context
	cancel  context.CancelFunc
	send    Send
	queue   chan []byte
	results chan Result
	done    chan struct{}
	count   int
	started bool
}

func New(ctx context.Context, maximum int, send Send) *Writer {
	ctx, cancel := context.WithCancel(ctx)
	return &Writer{
		ctx: ctx, cancel: cancel, send: send,
		queue: make(chan []byte, maximum), results: make(chan Result, maximum),
		done: make(chan struct{}),
	}
}

func (writer *Writer) Enqueue(body []byte) error {
	if err := writer.ctx.Err(); err != nil {
		return err
	}
	if writer.count >= cap(writer.queue) {
		return ErrLimit
	}
	writer.count++
	// Total submissions cannot exceed capacity, so publication never waits for
	// the transport or for the worker to be scheduled.
	writer.queue <- bytes.Clone(body)
	if !writer.started {
		writer.started = true
		go writer.run()
	}
	return nil
}

func (writer *Writer) Results() <-chan Result {
	if writer == nil {
		return nil
	}
	return writer.results
}

func (writer *Writer) Stop() {
	if writer != nil {
		writer.cancel()
	}
}

func (writer *Writer) Close() {
	if writer == nil {
		return
	}
	writer.Stop()
	if writer.started {
		<-writer.done
	}
}

func (writer *Writer) run() {
	defer close(writer.done)
	for {
		select {
		case <-writer.ctx.Done():
			return
		case body := <-writer.queue:
			if writer.ctx.Err() != nil {
				return
			}
			delivered, err := writer.send(writer.ctx, body)
			// One result per admitted body fits the lifetime quota. Preserve a
			// racing failure without blocking shutdown on the owner's inbox.
			writer.results <- Result{Delivered: delivered, Err: err}
			if err != nil {
				return
			}
		}
	}
}
