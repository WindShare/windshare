package protocolcontract

import (
	"slices"
	"testing"
)

func TestRenewLeaseHasOnlyItsFrozenFinals(t *testing.T) {
	finals := legalOperationFinals()["renew-lease"]
	if !slices.Equal(finals, []string{"lease-result", "operation-error"}) || slices.Contains(finals, "operation-complete") {
		t.Fatalf("renew finals = %v", finals)
	}
}

func TestExplicitStopHasAProtocolDistinctSessionCode(t *testing.T) {
	if sessionCodeSenderStopped != 0x1008 || sessionCodeSenderStopped == 0x1007 {
		t.Fatalf("sender-stopped code = %#x", sessionCodeSenderStopped)
	}
}
