//go:build linux || darwin

package outputportable

import (
	"errors"
	"io"
	"os"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

const nativeSpellingBatchSize = 128

// File identity cannot distinguish hard links. Match the requested spelling in
// the retained parent first, then bind that entry to the observed object.
func directoryEntryMatchesSpelling(parent *directory, name string, observed os.FileInfo) (exact bool, resultErr error) {
	handle, err := parent.root.Open(".")
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, handle.Close()) }()
	for {
		entries, readErr := handle.ReadDir(nativeSpellingBatchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return false, readErr
		}
		for _, entry := range entries {
			if !exactEntrySpelling(name, entry.Name()) {
				continue
			}
			candidate, statErr := entry.Info()
			if statErr != nil {
				return false, statErr
			}
			if !os.SameFile(observed, candidate) {
				return false, outputcap.ErrUnsafeNamespace
			}
			return true, nil
		}
		if errors.Is(readErr, io.EOF) {
			return false, nil
		}
	}
}
