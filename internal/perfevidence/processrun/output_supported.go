//go:build linux || windows

package processrun

import (
	"errors"
	"fmt"
	"io"
	"os"
)

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
