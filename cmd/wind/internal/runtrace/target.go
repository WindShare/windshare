package runtrace

import (
	"path/filepath"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

const (
	directoryCreateAttempts = 8
	// Exclusive creation and fresh-ID retries are the collision authority. A
	// 48-bit prefix keeps filenames readable while records retain the full run ID.
	directoryFilenameTokenHexLength = 12
	directoryTimestampLayout        = "20060102T150405Z"
	traceFilenameExtension          = ".ndjson"
)

type targetKind uint8

const (
	targetExactFile targetKind = iota + 1
	targetRunDirectory
)

// Target keeps the no-clobber policy coupled to the namespace the user chose.
// Its fields stay private so callers cannot create an ambiguous file-or-directory target.
type Target struct {
	kind targetKind
	path string
}

// ExactFile treats the caller's path as a one-attempt ownership claim so a
// collision cannot alter the prior run's evidence.
func ExactFile(path string) (Target, error) {
	return newTarget(targetExactFile, path)
}

// RunDirectory preserves prior evidence by assigning each invocation a fresh,
// internally generated sibling rather than applying overwrite or rotation policy.
func RunDirectory(path string) (Target, error) {
	return newTarget(targetRunDirectory, path)
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

func directoryTracePath(directory string, command clievent.Command, started time.Time, runID string) string {
	commandName, _ := command.Name()
	filenameToken := runID[:directoryFilenameTokenHexLength]
	filename := commandName + "-" + started.UTC().Format(directoryTimestampLayout) + "-" + filenameToken + traceFilenameExtension
	return filepath.Join(directory, filename)
}
