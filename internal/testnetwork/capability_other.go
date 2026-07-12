//go:build !windows

package testnetwork

import "os/exec"

func windowsHarnessAuthorized() bool {
	return false
}

func verifyWindowsAuthorizedExecutable(string) error {
	return nil
}

func startWindowsGuardedProcess(cmd *exec.Cmd) error {
	return cmd.Start()
}
