package protocolcontract

import (
	"slices"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestRenewLeaseHasOnlyItsFrozenFinals(t *testing.T) {
	finals := legalOperationFinals()["renew-lease"]
	if !slices.Equal(finals, []string{"lease-result", "operation-error"}) || slices.Contains(finals, "operation-complete") {
		t.Fatalf("renew finals = %v", finals)
	}
	if !protocolsession.MessageAllowedForOperation(protocolsession.MessageRenewLease, protocolsession.MessageLeaseResult) ||
		!protocolsession.MessageAllowedForOperation(protocolsession.MessageRenewLease, protocolsession.MessageOperationError) ||
		protocolsession.MessageAllowedForOperation(protocolsession.MessageRenewLease, protocolsession.MessageOperationComplete) {
		t.Fatal("production MessageAllowedForOperation diverged from renew-lease legal finals")
	}
}

func TestExplicitStopHasAProtocolDistinctSessionCode(t *testing.T) {
	if sessionCodeSenderStopped != 0x1008 || sessionCodeSenderStopped == 0x1007 {
		t.Fatalf("sender-stopped code = %#x", sessionCodeSenderStopped)
	}
}
