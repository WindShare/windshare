//go:build linux

package processowner_test

import (
	"testing"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

func requireStoppedTarget(t *testing.T, settlement protocol.Settlement) {
	t.Helper()
	if settlement.TerminationReason != protocol.TerminationStop ||
		settlement.Target.Outcome != protocol.TargetSignaled ||
		settlement.Target.Signal != "SIGTERM" {
		t.Fatalf("Linux stop settlement = %#v", settlement)
	}
}
