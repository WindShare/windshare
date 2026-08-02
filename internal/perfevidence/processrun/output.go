package processrun

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

const capturedOutputChunkBytes = 32 << 10

// processOutput owns one aggregate budget. A command cannot evade the memory
// bound by alternating writes between stdout and stderr.
type processOutput struct {
	budget       *outputBudget
	stdout       *boundedOutput
	stderr       *boundedOutput
	overflowOnce sync.Once
	overflowCh   chan struct{}
}

type outputBudget struct {
	mu        sync.Mutex
	remaining int
}

type boundedOutput struct {
	owner *processOutput

	mu       sync.Mutex
	chunks   [][]byte
	captured int
}

func newProcessOutput(maximum int) *processOutput {
	output := &processOutput{
		budget:     &outputBudget{remaining: maximum},
		overflowCh: make(chan struct{}),
	}
	output.stdout = &boundedOutput{owner: output}
	output.stderr = &boundedOutput{owner: output}
	return output
}

func (output *processOutput) overflow() <-chan struct{} {
	return output.overflowCh
}

func (output *processOutput) exceeded() bool {
	select {
	case <-output.overflowCh:
		return true
	default:
		return false
	}
}

func (output *processOutput) signalOverflow() {
	output.overflowOnce.Do(func() { close(output.overflowCh) })
}

func (output *processOutput) snapshot() ([]byte, []byte) {
	return output.stdout.snapshot(), output.stderr.snapshot()
}

func (output *boundedOutput) Write(value []byte) (int, error) {
	if len(value) == 0 {
		return 0, nil
	}
	output.owner.budget.mu.Lock()
	accepted := min(len(value), output.owner.budget.remaining)
	output.owner.budget.remaining -= accepted
	output.owner.budget.mu.Unlock()

	if accepted > 0 {
		output.mu.Lock()
		pending := value[:accepted]
		for len(pending) > 0 {
			if len(output.chunks) == 0 || len(output.chunks[len(output.chunks)-1]) == cap(output.chunks[len(output.chunks)-1]) {
				output.chunks = append(output.chunks, make([]byte, 0, min(capturedOutputChunkBytes, len(pending))))
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
		output.mu.Unlock()
	}
	if accepted != len(value) {
		// Draining continues after saturation so descendants cannot retain a full
		// pipe and deadlock authoritative tree settlement.
		output.owner.signalOverflow()
	}
	return len(value), nil
}

func (output *boundedOutput) snapshot() []byte {
	output.mu.Lock()
	defer output.mu.Unlock()
	result := make([]byte, output.captured)
	offset := 0
	for _, chunk := range output.chunks {
		offset += copy(result[offset:], chunk)
	}
	return result
}

func drainOutput(source io.Reader, destination *boundedOutput, label string) error {
	buffer := make([]byte, capturedOutputChunkBytes)
	for {
		count, readErr := source.Read(buffer)
		if count > 0 {
			written, writeErr := destination.Write(buffer[:count])
			if writeErr != nil {
				return fmt.Errorf("capture %s: %w", label, writeErr)
			}
			if written != count {
				return fmt.Errorf("capture %s: %w", label, io.ErrShortWrite)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, os.ErrClosed) {
				return nil
			}
			return fmt.Errorf("drain %s: %w", label, readErr)
		}
		if count == 0 {
			return fmt.Errorf("drain %s: %w", label, io.ErrNoProgress)
		}
	}
}
