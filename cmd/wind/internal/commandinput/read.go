// Package commandinput keeps command cancellation independent of blocking input.
package commandinput

import (
	"bufio"
	"context"
)

// Read waits for one input operation without letting a terminal, pipe, or file
// delay command cancellation. The operation must own its buffers and only return
// data; validation effects and commits belong to the caller after Read returns.
//
// An arbitrary input source cannot be interrupted portably. If cancellation
// wins, the pending read lives until its source is released or the CLI exits.
// The caller must not reuse that source after cancellation. A buffered result
// lets the reader finish without touching abandoned command state.
func Read[T any](ctx context.Context, read func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if ctx.Done() == nil {
		return read()
	}
	type result struct {
		value T
		err   error
	}
	ready := make(chan result, 1)
	go func() {
		value, err := read()
		ready <- result{value: value, err: err}
	}()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case response := <-ready:
		// Cancellation must not become a confirmation or a configuration commit
		// just because input became ready at the same time.
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		return response.value, response.err
	}
}

func ReadLine(ctx context.Context, reader *bufio.Reader) (string, error) {
	return Read(ctx, func() (string, error) { return reader.ReadString('\n') })
}
