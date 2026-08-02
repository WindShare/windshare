package processrun

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	helperRoleEnvironment = "WINDSHARE_PERFEVIDENCE_PROCESS_OWNER_ROLE"
	helperRoleValue       = "contained-owner-v1"
)

// MaybeRunHelper dispatches private owner modes before the embedding executable
// parses its public CLI. The target executable remains the caller-requested
// command; only the owner layers re-enter this binary.
func MaybeRunHelper(arguments []string, input io.Reader) (bool, int) {
	if !isOwnerCommand(arguments) {
		return false, 0
	}
	if os.Getenv(helperRoleEnvironment) != helperRoleValue && !allowsRolelessOwnerCommand(arguments) {
		return false, 0
	}
	if err := runOwnerCommand(arguments, input); err != nil {
		// The public process cannot otherwise reconstruct why an owner image exited
		// before its authenticated settlement channel became available.
		_, _ = fmt.Fprintf(os.Stderr, "windshare process-owner helper failed: %s\n", boundedDiagnostic(err))
		return true, 1
	}
	return true, 0
}

func ownerHelperEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment)+1)
	prefix := helperRoleEnvironment + "="
	for _, entry := range environment {
		if !strings.EqualFold(strings.SplitN(entry, "=", 2)[0], helperRoleEnvironment) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+helperRoleValue)
}
