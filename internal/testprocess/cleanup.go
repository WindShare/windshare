package testprocess

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

type CleanupTB interface {
	Helper()
	Cleanup(func())
	Errorf(format string, arguments ...any)
}

// RegisterCleanup makes process-tree settlement a test cleanup gate. It is safe
// to register for processes that may finish naturally before the test returns.
func RegisterCleanup(test CleanupTB, process *Process, timeout time.Duration) {
	test.Helper()
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		settlement, stopErr := process.Stop(ctx)
		cleanupErr := RequireTreeEmpty(settlement)
		if err := errors.Join(stopErr, cleanupErr); err != nil {
			test.Errorf("owned process cleanup: %v", err)
		}
	})
}

func (process *Process) StopAndRequireTreeEmpty(ctx context.Context) (protocol.Settlement, error) {
	settlement, stopErr := process.Stop(ctx)
	return settlement, errors.Join(stopErr, RequireTreeEmpty(settlement))
}
