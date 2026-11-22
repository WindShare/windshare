// Package diaglog provides bounded, non-blocking JSONL diagnostic recording
// shared by the desktop host and CLI with a single dropped-record counter.
package diaglog

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"time"
)

const (
	ownerOnlyDirectoryMode = 0o700
	ownerOnlyFileMode      = 0o600

	runIDBytes              = 16
	directoryCreateAttempts = 8
	filenameTokenHexLength  = 12
	directoryTimestampFmt   = "20060102T150405Z"
	runTraceExtension       = ".ndjson"
)

var (
	ErrInvalidTarget             = errors.New("trace target is invalid")
	ErrInvalidConfig             = errors.New("trace configuration is invalid")
	ErrRunIDUnavailable          = errors.New("trace run identity is unavailable")
	ErrTraceDirectoryUnavailable = errors.New("trace directory is unavailable")
	ErrTraceFileUnavailable      = errors.New("trace file is unavailable")
	ErrTraceNameUnavailable      = errors.New("trace filename is unavailable")
	ErrTraceExists               = errors.New("trace path already exists")
)

type targetKind uint8

const (
	targetExactFile targetKind = iota + 1
	targetRunDirectory
)

// Target couples the no-clobber policy to the file or directory namespace
// selected by the caller.
type Target struct {
	kind targetKind
	path string
}

// ExactFile claims a single new file path so collisions preserve prior evidence.
func ExactFile(path string) (Target, error) {
	return newTarget(targetExactFile, path)
}

// RunDirectory generates a unique file per invocation inside directory.
func RunDirectory(directory string) (Target, error) {
	return newTarget(targetRunDirectory, directory)
}

func newTarget(kind targetKind, path string) (Target, error) {
	if path == "" || path == "-" {
		return Target{}, ErrInvalidTarget
	}
	return Target{kind: kind, path: path}, nil
}

func (target Target) valid() bool {
	return target.path != "" && target.path != "-" &&
		(target.kind == targetExactFile || target.kind == targetRunDirectory)
}

func newRunID(random io.Reader) ([runIDBytes]byte, error) {
	if random == nil {
		random = rand.Reader
	}
	var raw [runIDBytes]byte
	if _, err := io.ReadFull(random, raw[:]); err != nil {
		return [runIDBytes]byte{}, err
	}
	allZero := true
	for _, b := range raw {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return [runIDBytes]byte{}, ErrRunIDUnavailable
	}
	return raw, nil
}

func encodeRunID(raw [runIDBytes]byte) string {
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func directoryRunPath(directory, command string, started time.Time, raw [runIDBytes]byte) string {
	token := hex.EncodeToString(raw[:])[:filenameTokenHexLength]
	name := command + "-" + started.UTC().Format(directoryTimestampFmt) + "-" + token + runTraceExtension
	return filepath.Join(directory, name)
}
