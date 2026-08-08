package resumecommand

import (
	"errors"
	"io"
)

type streamResumeOutput struct {
	result io.Writer
	usage  io.Writer
}

func (output streamResumeOutput) WriteResult(value string) error {
	if output.result == nil {
		return errors.New("standard output is unavailable")
	}
	written, err := io.WriteString(output.result, value)
	if err == nil && written != len(value) {
		return io.ErrShortWrite
	}
	return err
}

func (output streamResumeOutput) WriteUsage(value string) {
	if output.usage != nil {
		_, _ = io.WriteString(output.usage, value)
	}
}

type logFunc func(string, ...any)

func (log logFunc) Logf(format string, args ...any) {
	if log != nil {
		log(format, args...)
	}
}
