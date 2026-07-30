//go:build !windows

package testnetwork

import "os/exec"

func newOSNetworkChildAuthority(string, string) (string, func() error, error) {
	return "", func() error { return nil }, nil
}

func windowsHarnessAuthorized() bool {
	return false
}

func verifyWindowsAuthorizedExecutable(string) error {
	return nil
}

func startWindowsGuardedProcess(cmd *exec.Cmd) error {
	return cmd.Start()
}
