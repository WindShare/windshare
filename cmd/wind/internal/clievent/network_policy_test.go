package clievent

import (
	"testing"

	"github.com/windshare/windshare/internal/platformsetup"
)

func TestNetworkPolicyRecommendationsRequireRepairableEvidence(t *testing.T) {
	finding := platformsetup.Finding{Protocol: platformsetup.UDP, Verdict: platformsetup.Blocked, Reason: platformsetup.DefaultBlock}
	if _, err := NewNetworkPolicyObserved(CommandShare, platformsetup.Ask, finding, true); err != nil {
		t.Fatal(err)
	}
	if _, err := NewNetworkPolicyObserved(CommandShare, platformsetup.Skip, finding, true); err == nil {
		t.Fatal("ignored saved choice")
	}
	finding.Reason = platformsetup.ExplicitBlock
	if _, err := NewNetworkPolicyObserved(CommandShare, platformsetup.Ask, finding, true); err == nil {
		t.Fatal("suggested ineffective repair")
	}
	if _, err := NewNetworkPolicyObserved(0, platformsetup.Ask, finding, false); err == nil {
		t.Fatal("missing command accepted")
	}
	if (NetworkPolicyObserved{}).Validate() == nil {
		t.Fatal("zero event accepted")
	}
}
