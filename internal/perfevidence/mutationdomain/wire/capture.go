package wire

import (
	"bytes"
	"sync"
)

// BoundedCapture reports the caller's full write as consumed so a child
// process cannot deadlock on a diagnostic pipe after the retained prefix fills.
type BoundedCapture struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func NewBoundedCapture(limit int) *BoundedCapture {
	return &BoundedCapture{limit: limit}
}

func (capture *BoundedCapture) Write(content []byte) (int, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	remaining := capture.limit - capture.buffer.Len()
	if remaining > 0 {
		kept := min(len(content), remaining)
		_, _ = capture.buffer.Write(content[:kept])
	}
	if len(content) > remaining {
		capture.overflow = true
	}
	return len(content), nil
}

func (capture *BoundedCapture) Snapshot() []byte {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return bytes.Clone(capture.buffer.Bytes())
}

func (capture *BoundedCapture) Exceeded() bool {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.overflow
}
