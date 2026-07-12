// Command d5capabilityprobe exercises the Windows test-only authorization boundary
// without opening a socket or spawning a production child.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/windshare/windshare/internal/testnetwork"
)

const testListArgument = "-test.list=^Test"

var probeMode = "authorization"

func main() {
	switch probeMode {
	case "enumeration":
		runEnumerationProbe(false)
		return
	case "network-enumeration":
		runEnumerationProbe(true)
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "child-hold" {
		time.Sleep(time.Minute)
		return
	}
	if testnetwork.OSNetworkEnabled() {
		fmt.Println("authorized")
		if len(os.Args) == 3 && os.Args[1] == "hold-child" {
			child := exec.Command(os.Args[2], "child-hold")
			if err := testnetwork.StartGuardedProcess(child); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			fmt.Printf("child-pid=%d\n", child.Process.Pid)
			_ = child.Wait()
		}
		return
	}
	if err := testnetwork.VerifyAuthorizedExecutable(os.Args[0]); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	fmt.Println("unauthorized")
}

func runEnumerationProbe(attemptNetwork bool) {
	if len(os.Args) != 2 || os.Args[1] != testListArgument {
		fmt.Fprintln(os.Stderr, "enumeration arguments rejected")
		os.Exit(2)
	}
	for _, name := range []string{
		"WINDSHARE_D5_AUTHORIZATION_PIPE",
		"WINDSHARE_WINDOWS_OS_NETWORK",
		"WINDSHARE_D5_HARNESS_CAPABILITY",
		"WINDSHARE_D5_AUTHORIZATION_MANIFEST",
		"WINDSHARE_D5_E2E_LEASE_TOKEN",
		"WINDSHARE_D5_RUNNER_PIPE",
		"WINDSHARE_D5_CHILD_MANIFEST",
	} {
		if _, present := os.LookupEnv(name); present {
			fmt.Fprintf(os.Stderr, "enumeration inherited authority environment %s\n", name)
			os.Exit(3)
		}
	}
	if attemptNetwork {
		if testnetwork.OSNetworkEnabled() {
			fmt.Fprintln(os.Stderr, "enumeration unexpectedly acquired network authority")
			os.Exit(4)
		}
		fmt.Fprintln(os.Stderr, "enumeration network authorization denied")
		os.Exit(5)
	}
	fmt.Println("TestEnumerationProbe")
}
