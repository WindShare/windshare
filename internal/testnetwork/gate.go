// Package testnetwork keeps real Windows socket tests out of random go-build
// executables while preserving their full effect in the fixed-path harness.
package testnetwork

import (
	"os/exec"
	"runtime"
)

type skipper interface {
	Helper()
	Skip(args ...any)
}

// OSNetworkEnabledFor keeps platform selection pure without exposing a public
// authorization string. On Windows, the caller must already have verified the
// runner-issued process capability.
func OSNetworkEnabledFor(goos string, runnerAuthorized bool) bool {
	return goos != "windows" || runnerAuthorized
}

// OSNetworkEnabled reports whether this exact process owns a live runner
// capability. A copied environment label is insufficient on Windows.
func OSNetworkEnabled() bool {
	return OSNetworkEnabledFor(runtime.GOOS, windowsHarnessAuthorized())
}

// StableHarnessFor prevents non-Windows runs from adopting fixed artifact paths.
func StableHarnessFor(goos string, runnerAuthorized bool) bool {
	return goos == "windows" && runnerAuthorized
}

// StableHarness reports whether this exact process may use the fixed namespace.
func StableHarness() bool {
	return StableHarnessFor(runtime.GOOS, windowsHarnessAuthorized())
}

// VerifyAuthorizedExecutable binds a child launch to the runner's immutable
// program manifest before the child can inherit any fixed-path identity.
func VerifyAuthorizedExecutable(path string) error {
	return verifyWindowsAuthorizedExecutable(path)
}

// StartGuardedProcess closes the wrapper-crash gap for process-owning tests. The
// Windows implementation verifies the child hash and registers it with the live
// runner guard before returning ownership to the test.
func StartGuardedProcess(cmd *exec.Cmd) error {
	return startWindowsGuardedProcess(cmd)
}

// RequireOSNetwork classifies a test at the resource-ownership boundary. Keeping
// the gate in shared listener/peer/process constructors also protects future test
// cases that reuse those constructors.
func RequireOSNetwork(t skipper) {
	requireOSNetworkFor(t, runtime.GOOS, windowsHarnessAuthorized())
}

// AssertOSNetwork protects asynchronous helper owners that no longer have a
// testing.T available. The parent constructor gates first for a normal skip;
// this assertion makes direct or future reuse fail closed instead of dialing.
func AssertOSNetwork() {
	if !OSNetworkEnabled() {
		panic("real Windows OS-network helper escaped the fixed-path runner")
	}
}

func requireOSNetworkFor(t skipper, goos string, runnerAuthorized bool) {
	t.Helper()
	if !OSNetworkEnabledFor(goos, runnerAuthorized) {
		t.Skip("real Windows OS-network tests require scripts/d5-windows-performance.ps1 -Mode NetworkTests")
	}
}
