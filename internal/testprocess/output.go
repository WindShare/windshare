package testprocess

import (
	"errors"
	"fmt"
	"sync"
)

// MaximumCapturedOutputBytes bounds each owned stream independently so a noisy
// child cannot turn correctness-test diagnostics into unbounded process memory.
const MaximumCapturedOutputBytes = 4 << 20

// capturedOutputChunkBytes keeps lazy capture metadata bounded without making
// every quiet process reserve the full per-stream authority up front.
const capturedOutputChunkBytes = 32 << 10

// ErrOutputCaptureLimit reports that lifecycle cleanup succeeded but a stream's
// complete diagnostic content could not be retained within its owned budget.
var ErrOutputCaptureLimit = errors.New("owned process output capture limit exceeded")

// OutputSnapshot is a detached copy of one live process stream. Mutating Bytes
// cannot change later snapshots; lifecycle drains never execute caller code.
type OutputSnapshot struct {
	Bytes     []byte
	Truncated bool
}

func (snapshot OutputSnapshot) String() string { return string(snapshot.Bytes) }

type processOutput struct {
	stdout *boundedOutput
	stderr *boundedOutput
}

func newProcessOutput() *processOutput {
	return &processOutput{
		stdout: newBoundedOutput("stdout"),
		stderr: newBoundedOutput("stderr"),
	}
}

type boundedOutput struct {
	label string

	mu        sync.Mutex
	chunks    [][]byte
	captured  int
	truncated bool
	terminal  error
}

func newBoundedOutput(label string) *boundedOutput {
	return &boundedOutput{label: label}
}

func (output *boundedOutput) Write(value []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.truncated {
		return len(value), nil
	}
	remaining := MaximumCapturedOutputBytes - output.captured
	accepted := min(len(value), remaining)
	pending := value[:accepted]
	for len(pending) > 0 {
		if len(output.chunks) == 0 || len(output.chunks[len(output.chunks)-1]) == cap(output.chunks[len(output.chunks)-1]) {
			capacity := min(capturedOutputChunkBytes, MaximumCapturedOutputBytes-output.captured)
			output.chunks = append(output.chunks, make([]byte, 0, capacity))
		}
		last := len(output.chunks) - 1
		chunk := output.chunks[last]
		copied := min(len(pending), cap(chunk)-len(chunk))
		start := len(chunk)
		chunk = chunk[:start+copied]
		copy(chunk[start:], pending[:copied])
		output.chunks[last] = chunk
		output.captured += copied
		pending = pending[copied:]
	}
	if accepted < len(value) {
		output.truncated = true
		output.terminal = fmt.Errorf("%s: %w", output.label, ErrOutputCaptureLimit)
	}
	return len(value), nil
}

func (output *boundedOutput) terminalError() error {
	if output == nil {
		return nil
	}
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.terminal
}

func (output *boundedOutput) snapshot() OutputSnapshot {
	if output == nil {
		return OutputSnapshot{}
	}
	output.mu.Lock()
	defer output.mu.Unlock()
	bytes := make([]byte, output.captured)
	offset := 0
	for _, chunk := range output.chunks {
		offset += copy(bytes[offset:], chunk)
	}
	return OutputSnapshot{Bytes: bytes, Truncated: output.truncated}
}
