package main

import (
	"strings"
	"testing"
)

func TestSourceImportPolicyPreservesOwnership(t *testing.T) {
	for _, test := range []struct {
		source, dependency, reason string
	}{
		{"engine/task.go", "/connectivity", ""},
		{"engine/task.go", "/transport/webrtc", ""},
		{"engine/task.go", "/relay/protocol/v2", ""},
		{"engine/task.go", "/osfs", "shared engine"},
		{"engine/task.go", "/core/osfs/records", "shared engine"},
		{"engine/task.go", "/adapters/filesystem", "shared engine"},
		{"engine/task.go", "/internal/testsupport", "test-support"},
		{"engine/observation/events.go", "/connectivity/relayset", "observation contracts"},
		{"engine/observation/events.go", "/osfs", "observation contracts"},
		{"engine/observation/events.go", "/core/transfer", ""},
		{"engine/observation/events.go", "/internal/testrun", "test-support"},
		{"engine/observations/events.go", "/connectivity/relayset", ""},
		{"host/desktop/source.go", "/adapters/filesystem", ""},
		{"host/desktop/source.go", "/connectivity/relayset", "through engine"},
		{"host/desktop/source.go", "/internal/testrelaynative", "test-support"},
		{"cmd/wind/main.go", "/osfs", ""},
		{"cmd/wind/main.go", "/transport", "through engine"},
		{"cmd/wind/main.go", "/internal/testrun", "test-support"},
		{"cmd/windshare-host/main.go", "/internal/testsupport", "test-support"},
		{"cmd/testprocessowner/main.go", "/internal/testprocess", ""},
		{"cmd/testprocessowner/main.go", "/relay/protocol/v2", "through engine"},
		{"cmd/wind-extra/main.go", "/internal/testsupport", ""},
		{"cmd/wind/main.go", "/connectivity-extra", ""},
		{"engine/task.go", "/osfs-extra", ""},
		{"engine/task.go", "/core/osfs-extra", ""},
	} {
		t.Run(test.source+"->"+test.dependency, func(t *testing.T) {
			reason := sourceImportViolation(test.source, module+test.dependency)
			if test.reason == "" {
				if reason != "" {
					t.Fatalf("allowed dependency rejected: %s", reason)
				}
			} else if !strings.Contains(reason, test.reason) {
				t.Fatalf("violation=%q, want reason containing %q", reason, test.reason)
			}
		})
	}
}
