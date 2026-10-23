package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turncapacity"
	"github.com/windshare/windshare/connectivity/turnroute"
)

func TestTURNPlanMismatchCannotClaimReservation(t *testing.T) {
	credential, _ := iceconfig.Fixed("u", "p")
	relay, _ := iceconfig.NewRelay(iceconfig.Identity{Provider: "test", Account: "a", Profile: "p"},
		[]string{"turn:localhost:3478"}, credential)
	configuration, _ := iceconfig.New(nil, &relay, iceconfig.RelayCandidates)
	changedCredential, _ := iceconfig.Fixed("u", "changed")
	changed, _ := iceconfig.NewRelay(relay.Identity(), relay.URLs(), changedCredential)
	otherPlan, _ := turnroute.NewPlan(changed, nil)
	plan, _ := turnroute.NewPlan(relay, nil)
	for _, test := range []struct {
		name    string
		plan    *turnroute.Plan
		sockets int
	}{
		{name: "missing"},
		{name: "different authority", plan: otherPlan, sockets: 1},
		{name: "different cost", plan: plan, sockets: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := turncapacity.Request{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2},
				Service: relay.Identity(), Sockets: max(1, test.sockets)}
			reservation, err := turncapacity.NewPool().NewOwner().Acquire(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(reservation.Close)
			_, err = claimTURNNetwork(AttemptConfig{ProtocolSessionID: request.ProtocolSessionID,
				PeerPathID: request.PeerPathID, ICE: configuration, TURN: &TURNConfig{Plan: test.plan, Reservation: reservation}})
			if !errors.Is(err, errTURNConfiguration) {
				t.Fatal("mismatched plan accepted", err)
			}
			if err := reservation.Claim(request.ProtocolSessionID, request.PeerPathID, request.Service); err != nil {
				t.Fatal("rejected plan consumed reservation", err)
			}
		})
	}
	if _, err := claimTURNNetwork(AttemptConfig{ICE: configuration}); !errors.Is(err, errTURNConfiguration) {
		t.Fatal("relayed attempt accepted without admission", err)
	}
	if _, err := claimTURNNetwork(AttemptConfig{TURN: &TURNConfig{Plan: plan}}); !errors.Is(err, errTURNConfiguration) {
		t.Fatal("direct attempt accepted TURN resources", err)
	}
}
