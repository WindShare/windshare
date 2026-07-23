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

func TestZipFailureEscalatesOnlyAfterMemberStart(t *testing.T) {
	if action, outcome := zipMemberFailure(false); action != "skip-and-report" || outcome != "completed-with-errors" {
		t.Fatalf("not-started ZIP member = (%q, %q)", action, outcome)
	}
	if action, outcome := zipMemberFailure(true); action != "abort-job" || outcome != "aborted" {
		t.Fatalf("started ZIP member = (%q, %q)", action, outcome)
	}
}
