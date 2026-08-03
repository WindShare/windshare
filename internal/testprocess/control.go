package testprocess

import (
	"errors"
	"fmt"
	"io"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

type controlPublicationError struct {
	cause        error
	bytesWritten int64
}

func (failure *controlPublicationError) Error() string { return failure.cause.Error() }
func (failure *controlPublicationError) Unwrap() error { return failure.cause }

func (failure *controlPublicationError) retryable() bool {
	return failure != nil && failure.bytesWritten == 0
}

type countingWriter struct {
	writer  io.Writer
	written int64
}

func (writer *countingWriter) Write(value []byte) (int, error) {
	written, err := writer.writer.Write(value)
	writer.written += int64(written)
	return written, err
}

func publishControl(writer io.Writer, control protocol.Control) error {
	counting := &countingWriter{writer: writer}
	if err := protocol.WriteFrame(counting, control); err != nil {
		return &controlPublicationError{
			cause:        fmt.Errorf("request process-owner stop: %w", err),
			bytesWritten: counting.written,
		}
	}
	return nil
}

func retryableControlPublication(err error) bool {
	var failure *controlPublicationError
	return errors.As(err, &failure) && failure.retryable()
}
