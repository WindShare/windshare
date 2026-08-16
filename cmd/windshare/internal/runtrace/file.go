package runtrace

import (
	"crypto/rand"
	"io"
	"os"
	"time"
)

const ownerOnlyFileMode = 0o600

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type systemTicker struct{ ticker *time.Ticker }

func (ticker systemTicker) C() <-chan time.Time { return ticker.ticker.C }
func (ticker systemTicker) Stop()               { ticker.ticker.Stop() }

type durableFile struct{ file *os.File }

func (file durableFile) Write(data []byte) (int, error) { return file.file.Write(data) }
func (file durableFile) Flush() error                   { return file.file.Sync() }
func (file durableFile) Close() error                   { return file.file.Close() }

func (file durableFile) Rollback(offset int64) error {
	if err := file.file.Truncate(offset); err != nil {
		return err
	}
	_, err := file.file.Seek(offset, io.SeekStart)
	return err
}

func normalizedDependencies(dependencies Dependencies) Dependencies {
	if dependencies.Clock == nil {
		dependencies.Clock = systemClock{}
	}
	if dependencies.Random == nil {
		dependencies.Random = rand.Reader
	}
	if dependencies.OpenFile == nil {
		dependencies.OpenFile = openOwnerOnlyFile
	}
	if dependencies.NewTicker == nil {
		dependencies.NewTicker = func(interval time.Duration) Ticker {
			return systemTicker{ticker: time.NewTicker(interval)}
		}
	}
	return dependencies
}

func openOwnerOnlyFile(path string) (TraceFile, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, ownerOnlyFileMode)
	if err != nil {
		return nil, err
	}
	// Tighten an existing file before truncating it so a failed permission
	// transition does not destroy the prior trace.
	if err := file.Chmod(ownerOnlyFileMode); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Truncate(0); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = file.Close()
		return nil, err
	}
	return durableFile{file: file}, nil
}
