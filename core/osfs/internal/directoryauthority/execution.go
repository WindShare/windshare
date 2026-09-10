package directoryauthority

import (
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

// directoryExecution is disposable authority, independent of the historical
// claim receipt. Borrowers pin its witness until their guarded operation ends.
type directoryExecution struct {
	gate     sync.RWMutex
	retained outputcap.Directory

	snapshotOnce sync.Once
	snapshot     parentNamespaceIndex
	snapshotErr  error
}

func newDirectoryExecution(retained outputcap.Directory) *directoryExecution {
	if retained == nil {
		return nil
	}
	return &directoryExecution{retained: retained}
}

func (execution *directoryExecution) close() error {
	if execution == nil {
		return nil
	}
	execution.gate.Lock()
	defer execution.gate.Unlock()
	retained := execution.retained
	execution.retained = nil
	return closeDirectory(retained)
}

type directoryWitness struct {
	claim     directoryClaim
	execution *directoryExecution
}

func borrowDirectoryLineage(lineage []directoryWitness) (func(), error) {
	borrowed := 0
	release := func() {
		for borrowed > 0 {
			borrowed--
			lineage[borrowed].execution.gate.RUnlock()
		}
	}
	for _, witness := range lineage {
		witness.execution.gate.RLock()
		borrowed++
		if witness.execution.retained == nil {
			release()
			return nil, ErrParentUnavailable
		}
	}
	return release, nil
}
