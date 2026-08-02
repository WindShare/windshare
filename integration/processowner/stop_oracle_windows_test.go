//go:build windows

package processowner_test

import (
	"testing"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

func requireStoppedTarget(t *testing.T, settlement protocol.Settlement) {
	t.Helper()
	if settlement.TerminationReason != protocol.TerminationStop ||
		settlement.Target.Outcome != protocol.TargetExited {
		t.Fatalf("Windows stop settlement = %#v", settlement)
	}
}
