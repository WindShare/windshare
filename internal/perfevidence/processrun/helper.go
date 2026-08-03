package processrun

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/text/unicode/norm"
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

func boundedDiagnostic(err error) string {
	message := "start authority rejected"
	if err != nil {
		message = strings.ReplaceAll(err.Error(), "\x00", " ")
		message = norm.NFC.String(message)
	}
	if message == "" {
		message = "start authority rejected"
	}
	if len(message) > protocol.MaximumDiagnosticBytes {
		message = message[:protocol.MaximumDiagnosticBytes]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}
