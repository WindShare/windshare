//go:build !windows

package windowsjob

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestUnsupportedPlatformRejectsSupervisorAndLauncher(t *testing.T) {
	t.Parallel()
	request := validSupervisionRequest(t)
	for _, test := range []struct {
		name string
		run  func() error
	}{
		{
			name: "supervisor",
			run: func() error {
				return runSupervisorPlatform(
					request,
					nil,
					nil,
					nil,
					nil,
					io.Discard,
				)
			},
		},
		{
			name: "launcher",
			run: func() error {
				return runLauncherPlatform(request.Protocol, 1, 0, bytes.NewReader(nil))
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
