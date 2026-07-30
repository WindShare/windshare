// Package testnetwork keeps real Windows socket tests out of random go-build
// executables while preserving their full effect in the fixed-path harness.
package testnetwork

import (
	"os/exec"
	"runtime"
)

const launchAuthorizationPipeEnvironment = "WINDSHARE_D5_AUTHORIZATION_PIPE"

// OSNetworkChildAuthority is a one-use delegation from an authenticated test
// process to one exact child executable and operation. The environment value is
// only a local pipe address; authority is issued after the server verifies the
// connecting PID, executable, and operation binding.
type OSNetworkChildAuthority struct {
	pipeName string
	retire   func() error
}

// NewOSNetworkChildAuthority preserves the per-process D5 boundary for fixtures
// that must acquire a resource from inside a supervised child process.
func NewOSNetworkChildAuthority(executable, operationID string) (OSNetworkChildAuthority, error) {
	pipeName, retire, err := newOSNetworkChildAuthority(executable, operationID)
	if err != nil {
		return OSNetworkChildAuthority{}, err
	}
	return OSNetworkChildAuthority{pipeName: pipeName, retire: retire}, nil
}

// EnvironmentVariable returns the sole non-secret address needed by the child
// to perform its PID-bound one-use authorization handshake.
func (a OSNetworkChildAuthority) EnvironmentVariable() (string, string) {
	return launchAuthorizationPipeEnvironment, a.pipeName
}

// Retire closes the parent-held guard. A child that has not authenticated can
// no longer consume the grant; an authenticated child loses its live authority.
func (a *OSNetworkChildAuthority) Retire() error {
	if a == nil || a.retire == nil {
		return nil
	}
	retire := a.retire
	a.retire = nil
	return retire()
}

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
