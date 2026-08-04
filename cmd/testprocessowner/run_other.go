//go:build !linux && !windows

package main

import (
	"errors"

	"github.com/windshare/windshare/internal/processowner"
)

func runPlatform([]string, processowner.Config) error {
	return errors.New("testprocessowner supports only Linux and Windows")
}
