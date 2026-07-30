//go:build !windows

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestUnsupportedPlatformRejectsSupervisorAndLauncher(t *testing.T) {
	t.Parallel()
	request := validStartRequest(t)
	for _, test := range []struct {
		name string
		run  func() error
	}{
		{
			name: "supervisor",
			run: func() error {
				return runSupervisorPlatform(
					request,
					"/tmp/status.json",
					"/tmp/control.bin",
					bytes.NewReader(nil),
				)
			},
		},
		{
			name: "launcher",
			run: func() error {
				return runLauncherPlatform(request, 1, 0, bytes.NewReader(nil))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.run()
			if err == nil || !strings.Contains(err.Error(), "only on Windows") {
				t.Fatalf("unsupported-platform error = %v", err)
			}
		})
	}
}
