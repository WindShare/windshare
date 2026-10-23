package nativepeer

import (
	"context"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turncapacity"
)

func TestTURNPreparedReplacementKeepsLivePredecessorAtProcessCapacity(t *testing.T) {
	_, _, makeNative := newAdmissionFixture(t, 0)
	native := makeNative()
	capacity := turncapacity.NewPool()
	native.turnResources = capacity.NewOwner()
	credential, _ := iceconfig.Fixed("u", "p")
	relay, _ := iceconfig.NewRelay(iceconfig.Identity{Provider: "test", Account: "a", Profile: "p"},
		[]string{"turn:127.0.0.1:3478"}, credential)
	config, _ := iceconfig.New(nil, &relay, iceconfig.RelayCandidates)
	request := attemptFor(1)
	request.ICE = config
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	old, err := native.NewPeerConnection(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	native.SetAdmitted(request.ProtocolSessionID, request.Binding.PeerPathID, false)
	for i := 1; i < turncapacity.MaximumConnections; i++ {
		reservation, err := native.turnResources.Acquire(ctx, turncapacity.Request{
			ProtocolSessionID: [16]byte{byte(i + 1)}, PeerPathID: [16]byte{1},
			Service: relay.Identity(), Sockets: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer reservation.Close()
	}
	nextRequest := request
	nextRequest.ReplacesPeerPathID = [16]byte(request.Binding.PeerPathID)
	nextRequest.Binding.PeerPathID[0]++
	nextRequest.Binding.AttemptID[0]++
	next, err := native.NewPeerConnection(ctx, nextRequest)
	if err != nil {
		t.Fatalf("replacement at ordinary capacity: %v", err)
	}
	defer next.Close()
	if old.SignalingState() == pion.SignalingStateClosed {
		t.Fatal("admission retired the predecessor")
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	native.SetAdmitted(nextRequest.ProtocolSessionID, nextRequest.Binding.PeerPathID, false)
	thirdRequest := nextRequest
	thirdRequest.ReplacesPeerPathID = [16]byte(nextRequest.Binding.PeerPathID)
	thirdRequest.Binding.PeerPathID[0]++
	thirdRequest.Binding.AttemptID[0]++
	third, err := native.NewPeerConnection(ctx, thirdRequest)
	if err != nil {
		t.Fatalf("settled handoff retained temporary capacity: %v", err)
	}
	defer third.Close()
}
