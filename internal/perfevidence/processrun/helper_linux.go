//go:build linux

package processrun

import (
	"io"

	"github.com/windshare/windshare/internal/processowner/linuxsubreaper"
)

func isOwnerCommand(arguments []string) bool {
	if len(arguments) != 1 {
		return false
	}
	switch arguments[0] {
	case "guard", "supervise", "exec-child":
		return true
	default:
		return false
	}
}

func allowsRolelessOwnerCommand(arguments []string) bool {
	// The supervisor deliberately execs the gate with an empty environment so
	// ambient controls cannot cross the pre-exec boundary. Its mandatory private
	// descriptors authenticate this one role more strongly than an environment marker.
	return len(arguments) == 1 && arguments[0] == "exec-child"
}

func runOwnerCommand(arguments []string, _ io.Reader) error {
	return linuxsubreaper.Run(arguments)
}
