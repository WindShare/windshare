//go:build !linux && !windows

package processrun

import (
	"errors"
	"io"
)

func isOwnerCommand([]string) bool { return false }

func allowsRolelessOwnerCommand([]string) bool { return false }

func runOwnerCommand([]string, io.Reader) error {
	return errors.New("process ownership is unsupported on this platform")
}
