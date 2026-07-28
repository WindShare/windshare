//go:build !linux && !windows

package outputruntime

import (
	"runtime"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func openOutputRuntimeTestPlatform(string, bool) (outputcap.Platform, error) {
	return nil, outputcap.ErrRecoverableOutputUnsupported
}

func newRuntimeTestRootSpec(t testing.TB) runtimeTestRootSpec {
	t.Helper()
	t.Skipf("certified durable-output test roots are unsupported on %s", runtime.GOOS)
	return runtimeTestRootSpec{}
}
