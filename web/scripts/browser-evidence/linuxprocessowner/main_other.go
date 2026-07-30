//go:build !linux

package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "self-check" {
		fmt.Fprintln(os.Stderr, "linux process owner is unsupported on this platform")
		os.Exit(1)
	}
	panic(errors.New("linux process owner is unsupported on this platform"))
}
