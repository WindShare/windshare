// Command e2e is the test-only WindShare process entry. Production builds use
// cmd/windshare and therefore cannot parse or discover a test ICE topology.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/windshare/windshare/cmd/windshare/internal/cli"
)

func main() {
	os.Exit(runE2E(
		os.Args[1:], os.Stderr,
		defaultE2EDependencies(), cli.RunProcess,
	))
}

func runE2E(
	args []string,
	stderr io.Writer,
	dependencies e2eDependencies,
	runProcess func([]string, cli.ProcessConfig) int,
) int {
	prepared, err := prepareE2E(args, dependencies)
	if err != nil {
		fmt.Fprintf(stderr, "windshare-e2e: %v\n", err)
		writeE2EUsage(stderr)
		return cli.ExitUsage
	}
	code := runProcess(prepared.command, cli.ProcessConfig{
		SenderPeerFactories: prepared.provider,
		SenderPeerEvidence:  prepared.evidence,
	})
	if err := prepared.evidence.Close(); err != nil {
		fmt.Fprintf(stderr, "windshare-e2e: close sender evidence: %v\n", err)
		if code == cli.ExitOK {
			return cli.ExitFailure
		}
	}
	return code
}

func writeE2EUsage(writer io.Writer) {
	fmt.Fprintln(writer, "Usage: windshare-e2e --test-ice-topology <absolute profile.json> --test-ice-topology-resolution <absolute resolution.json> --test-ice-topology-profile-sha256 <sha256> --test-ice-topology-resolution-sha256 <sha256> --sender-evidence <absolute attempts.jsonl> <command> [args...]")
}
