//go:build linux

package main

import "github.com/windshare/windshare/internal/processowner/linuxsubreaper"

var runLinux = linuxsubreaper.Run

func runPlatform(arguments []string) error {
	return runLinux(arguments)
}
