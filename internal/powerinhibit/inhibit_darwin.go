//go:build darwin

package powerinhibit

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

const caffeinateBinary = "/usr/bin/caffeinate"

// acquireSystemSleepInhibition spawns macOS's built-in caffeinate without CGo
// (matching CGO_ENABLED=0 release builds). Passing -i prevents idle system
// sleep while allowing display sleep, and -w ties assertion lifetime to this PID.
func acquireSystemSleepInhibition(_ string) (func(), error) {
	cmd := exec.Command(caffeinateBinary, "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start caffeinate: %w", err)
	}
	return func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}, nil
}
