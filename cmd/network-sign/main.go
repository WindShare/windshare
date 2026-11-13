// network-sign keeps root authorization and online directory publication local.
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
		return fmt.Errorf("usage: network-sign keygen|authorize|sign|verify [options]")
	}
	switch args[0] {
	case "keygen":
		return keygen(args[1:])
	case "authorize":
		return authorize(args[1:])
	case "sign":
		return sign(args[1:])
	case "verify":
		return verify(args[1:])
	default:
		return fmt.Errorf("unknown network signing operation")
	}
}
