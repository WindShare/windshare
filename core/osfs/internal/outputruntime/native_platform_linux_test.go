//go:build linux

package outputruntime

import (
	"testing"

	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputlinux"
)

func openNativeOutputRuntimeTestPlatform(path string, create bool) (outputcap.Platform, error) {
	return outputlinux.Open(path, create)
}

func newNativeRuntimeTestRootSpec(t testing.TB) runtimeTestRootSpec {
	t.Helper()
	requireDurableFilesystemScenario(t)
	fixture := testoutputroot.New(t)
	return runtimeTestRootSpec{path: fixture.RootPath, create: fixture.CreateRoot}
}
