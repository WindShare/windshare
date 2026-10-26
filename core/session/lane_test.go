package sessioncontract

import (
	"strings"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestLaneIdentityReservesZeroEpochForTranscript(t *testing.T) {
	for _, test := range []struct {
		identity       LaneIdentity
		initial, valid bool
	}{
		{LaneIdentity{}, true, false}, {LaneIdentity{Epoch: 1}, false, false},
		{LaneIdentity{ID: 7}, true, true}, {LaneIdentity{ID: 7}, false, false},
		{LaneIdentity{ID: 7, Epoch: 2}, false, true},
	} {
		if valid := test.identity.Valid(test.initial); valid != test.valid {
			t.Fatalf("identity=%+v initial=%v valid=%v", test.identity, test.initial, valid)
		}
	}
}

func TestLaneRejectionRetainsAuthenticatedReason(t *testing.T) {
	failure := &LaneRejectedError{Rejection: protocolsession.LaneRejection{Code: protocolsession.LaneRejectAdmissionLimited}}
	if !strings.Contains(failure.Error(), "code") {
		t.Fatal("lane rejection omitted its reason")
	}
}
