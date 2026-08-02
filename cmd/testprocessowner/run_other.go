//go:build !windows && !linux

package main

import (
	"fmt"
	"runtime"
)

func runPlatform([]string) error {
	return fmt.Errorf("testprocessowner is unsupported on %s", runtime.GOOS)
}
