//go:build linux

package linuxsubreaper

import (
	"errors"
	"fmt"
	"os"
)

const (
	commandSupervise        = "supervise"
	commandGuard            = "guard"
	commandExecChild        = "exec-child"
	statusDescriptor        = 3
	controlDescriptor       = 4
	childInputDescriptor    = 5
	startEvidenceDescriptor = 7
	startDecisionDescriptor = 8
)

func main() {
	if err := runMain(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, boundedDiagnostic(err))
		os.Exit(1)
	}
}

// Run dispatches the public guardian and its private owner/exec containment
// layers. Clients enter through guard so a stalled owner never remains the last
// process capable of adopting and retiring its descendants.
func Run(arguments []string) error {
	return runMain(arguments)
}

func runMain(arguments []string) error {
	if len(arguments) != 1 {
		return errors.New("linux process owner requires exactly one command")
	}
	switch arguments[0] {
	case commandGuard:
		return runGuard(arguments)
	case commandSupervise:
		return runSupervise(arguments)
	case commandExecChild:
		return runExecChild()
	default:
		return fmt.Errorf("unknown linux process owner command %q", arguments[0])
	}
}
