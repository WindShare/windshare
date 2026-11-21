// network-sign authorizes signing keys offline and mirrors authority-signed publications.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: network-sign keygen|authorize|export|mirror|verify [options]")
	}
	switch args[0] {
	case "keygen":
		return keygen(args[1:])
	case "authorize":
		return authorize(args[1:])
	case "export":
		return exportDirectory(args[1:])
	case "mirror":
		return mirror(args[1:])
	case "verify":
		return verify(args[1:])
	default:
		return fmt.Errorf("unknown network signing operation")
	}
}
