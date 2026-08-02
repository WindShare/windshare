//go:build windows

package processrun

import (
	"io"

	"github.com/windshare/windshare/internal/processowner/windowsjob"
)

func isOwnerCommand(arguments []string) bool {
	if len(arguments) == 0 {
		return false
	}
	return arguments[0] == "supervise" || arguments[0] == "launcher"
}

func allowsRolelessOwnerCommand([]string) bool { return false }

func runOwnerCommand(arguments []string, input io.Reader) error {
	return windowsjob.Run(arguments, input)
}
