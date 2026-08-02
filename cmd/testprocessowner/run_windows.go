//go:build windows

package main

import (
	"io"
	"os"

	"github.com/windshare/windshare/internal/processowner/windowsjob"
)

var ownerInput io.Reader = os.Stdin

func runPlatform(arguments []string) error {
	return windowsjob.Run(arguments, ownerInput)
}
