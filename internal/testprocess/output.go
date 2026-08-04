package testprocess

import (
	"bytes"
	"context"
	"regexp"
	"sync"
)

const MaximumCapturedOutputBytes = 1 << 20

type OutputStream string

const (
	Stdout OutputStream = "stdout"
	Stderr OutputStream = "stderr"
)

type OutputSnapshot struct {
	Bytes     []byte
	Truncated bool
}

func (snapshot OutputSnapshot) String() string { return string(snapshot.Bytes) }

type boundedOutput struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
	changed   chan struct{}
}

func newBoundedOutput() *boundedOutput {
	return &boundedOutput{changed: make(chan struct{})}
}

func (output *boundedOutput) Write(value []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	combined := append(append([]byte(nil), output.data...), value...)
	if len(combined) > MaximumCapturedOutputBytes {
		combined = append([]byte(nil), combined[len(combined)-MaximumCapturedOutputBytes:]...)
		output.truncated = true
	}
	output.data = combined
	close(output.changed)
	output.changed = make(chan struct{})
	return len(value), nil
}

func (output *boundedOutput) snapshot() OutputSnapshot {
	output.mu.Lock()
	defer output.mu.Unlock()
	return OutputSnapshot{Bytes: bytes.Clone(output.data), Truncated: output.truncated}
}

func (output *boundedOutput) snapshotAndChange() (OutputSnapshot, <-chan struct{}) {
	output.mu.Lock()
	defer output.mu.Unlock()
	return OutputSnapshot{Bytes: bytes.Clone(output.data), Truncated: output.truncated}, output.changed
}

func (output *boundedOutput) waitFor(
	ctx context.Context,
	done <-chan struct{},
	pattern *regexp.Regexp,
) ([]string, error) {
	for {
		snapshot, changed := output.snapshotAndChange()
		if match := pattern.FindStringSubmatch(snapshot.String()); match != nil {
			return match, nil
		}
		select {
		case <-changed:
		case <-done:
			if match := pattern.FindStringSubmatch(output.snapshot().String()); match != nil {
				return match, nil
			}
			return nil, errReadinessBeforeExit
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
