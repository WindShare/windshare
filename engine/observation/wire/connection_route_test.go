package wire_test

import (
	"bytes"
	"testing"

	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
)

func TestSenderAdmissionRouteSurvivesProjectionAndEncoding(t *testing.T) {
	session, _ := appobs.NewProtocolSessionID(bytes.Repeat([]byte{1}, 16))
	grant, _ := appobs.NewProtocolOperationID(bytes.Repeat([]byte{2}, 16))
	lane, _ := appobs.NewLaneIdentity(2, 1)
	path, _ := appobs.NewPeerPathID(bytes.Repeat([]byte{3}, 16))
	attempt, _ := appobs.NewPeerAttemptID(bytes.Repeat([]byte{4}, 16))
	source := appobs.PeerAttemptSpec{
		Workflow: appobs.WorkflowShare, Session: session, PeerPath: path, Attempt: attempt,
		Sequence: 1, Stage: appobs.PeerAttemptStarted,
	}
	assertRoute := func(source appobs.PeerAttemptSpec, want string) {
		t.Helper()
		event, err := appobs.NewPeerAttemptObserved(source)
		if err != nil {
			t.Fatal(err)
		}
		record, err := wire.Encode(event)
		if err != nil {
			t.Fatal(err)
		}
		if payload := record.Payload.(wire.PeerAttemptPayload); payload.Route != want {
			t.Fatalf("route changed at observation boundary: got %q want %q", payload.Route, want)
		}
	}
	assertRoute(source, "")
	source.Route = appobs.LaneRouteDirect
	if _, err := appobs.NewPeerAttemptObserved(source); err == nil {
		t.Fatal("unadmitted attempt claimed a selected route")
	}
	source.Stage, source.Phase = appobs.PeerAttemptAdmitted, appobs.PeerPhaseAdmission
	source.GrantOperation, source.Lane = grant, lane
	source.HasGrantOperation, source.HasLane = true, true
	source.AdmissionDisposition, source.ResponseDelivery = appobs.PeerAdmissionAccepted, appobs.PeerResponseDelivered
	for _, test := range []struct {
		route appobs.LaneRoute
		name  string
	}{{appobs.LaneRouteDirect, "direct"}, {appobs.LaneRouteTURN, "turn"}} {
		source.Route = test.route
		assertRoute(source, test.name)
	}
	for _, invalid := range []appobs.LaneRoute{0, appobs.LaneRouteRelay, 255} {
		source.Route = invalid
		if _, err := appobs.NewPeerAttemptObserved(source); err == nil {
			t.Fatalf("invalid WebRTC admission route accepted: %d", invalid)
		}
	}
}

func TestLaneSettlementPreservesAllRouteKinds(t *testing.T) {
	session, _ := appobs.NewProtocolSessionID(bytes.Repeat([]byte{1}, 16))
	for _, test := range []struct {
		route appobs.LaneRoute
		name  string
	}{{appobs.LaneRouteRelay, "relay"}, {appobs.LaneRouteDirect, "direct"}, {appobs.LaneRouteTURN, "turn"}} {
		event, err := appobs.NewLaneSettlementObserved(appobs.LaneSettlementSpec{
			Session: session, Lane: func() appobs.LaneIdentity { v, _ := appobs.NewLaneIdentity(2, 1); return v }(),
			Route: test.route, DeliveredBlocks: 3, DeliveredBytes: 1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		record, err := wire.Encode(event)
		if err != nil {
			t.Fatal(err)
		}
		payload := record.Payload.(wire.LaneSettlementPayload)
		if payload.Route != test.name || payload.DeliveredBlocks != "3" || payload.DeliveredBytes != "1024" {
			t.Fatalf("settlement lost path or accounting: %+v", payload)
		}
	}
}
