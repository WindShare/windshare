package provider

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/ice/v4"
)

func TestLatePeerReflexiveCheckRemainsPossibleInsideInitialWindow(t *testing.T) {
	const initialWindow = 500 * time.Millisecond
	makeAgent := func() *ice.Agent {
		conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn})
		t.Cleanup(func() { _ = mux.Close() })
		agent, err := ice.NewAgentWithOptions(ice.WithUDPMux(mux), ice.WithIncludeLoopback(), ice.WithNetworkTypes([]ice.NetworkType{ice.NetworkTypeUDP4}),
			ice.WithDisconnectedTimeout(10*time.Millisecond), ice.WithFailedTimeout(10*time.Millisecond),
			ice.WithCheckInterval(5*time.Millisecond), ice.WithProviderConfig(ice.ProviderConfig{InitialCheckingTimeout: initialWindow}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = agent.Close() })
		complete := make(chan struct{})
		if err = agent.OnCandidate(func(candidate ice.Candidate) {
			if candidate == nil {
				close(complete)
			}
		}); err != nil {
			t.Fatal(err)
		}
		if err = agent.GatherCandidates(); err != nil {
			t.Fatal(err)
		}
		await(t, complete)
		return agent
	}
	left, right := makeAgent(), makeAgent()
	leftUser, leftPass, _ := left.GetLocalUserCredentials()
	rightUser, rightPass, _ := right.GetLocalUserCredentials()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type result struct {
		conn *ice.Conn
		err  error
	}
	leftResult, rightResult := make(chan result, 1), make(chan result, 1)
	go func() { conn, err := left.Dial(ctx, rightUser, rightPass); leftResult <- result{conn, err} }()
	go func() { conn, err := right.Accept(ctx, leftUser, leftPass); rightResult <- result{conn, err} }()
	// The ordinary connected failure budget is already exhausted before either
	// side has a useful pair. Only one side receives a late candidate; the other
	// must discover a peer-reflexive path from the authenticated inbound check.
	time.Sleep(60 * time.Millisecond)
	candidates, err := right.GetLocalCandidates()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if err = left.AddRemoteCandidate(candidate); err != nil {
			t.Fatal(err)
		}
	}
	acceptedLeft, acceptedRight := await(t, leftResult), await(t, rightResult)
	if acceptedLeft.err != nil || acceptedRight.err != nil {
		t.Fatalf("late checks failed: %v %v", acceptedLeft.err, acceptedRight.err)
	}
	pair, err := right.GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Remote.Type() != ice.CandidateTypePeerReflexive {
		t.Fatalf("late prflx=%v err=%v", pair, err)
	}
}
