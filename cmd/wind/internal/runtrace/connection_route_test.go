package runtrace

import (
	"bytes"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
)

func TestSenderAdmissionRouteSurvivesProjectionAndEncoding(t *testing.T) {
	session, _ := protocolsession.ProtocolSessionIDFromBytes(bytes.Repeat([]byte{1}, 16))
	grant, _ := protocolsession.OperationIDFromBytes(bytes.Repeat([]byte{2}, 16))
	lane := sessionruntime.LaneIdentity{ID: 2, Epoch: 1}
	source := v2peer.SenderAttemptObservation{
		SessionID: session, PeerPathID: v2signal.PeerPathID{3}, AttemptID: v2signal.AttemptID{4},
		SideSequence: 1, Stage: v2peer.SenderAttemptStarted,
	}
	assertRoute := func(source v2peer.SenderAttemptObservation, want string) {
		t.Helper()
		event, err := commandprojection.ProjectSenderAttempt(clievent.CommandShare, source)
		if err != nil {
			t.Fatal(err)
		}
		record, err := encodeV4(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(1, 0)}, event)
		if err != nil {
			t.Fatal(err)
		}
		if payload := record.Payload.(peerAttemptPayloadV4); payload.Route != want {
			t.Fatalf("route changed at observation boundary: got %q want %q", payload.Route, want)
		}
	}
	assertRoute(source, "")
	source.Route = transfer.LaneRouteDirect
	if _, err := commandprojection.ProjectSenderAttempt(clievent.CommandShare, source); err == nil {
		t.Fatal("unadmitted attempt claimed a selected route")
	}
	source.Stage, source.Phase = v2peer.SenderAttemptAdmitted, v2peer.SenderAttemptPhaseAdmission
	source.GrantOperationID, source.Lane = grant, &lane
	source.AdmissionDisposition, source.ResponseDelivery = v2peer.SenderAdmissionAccepted, v2peer.SenderResponseDelivered
	for _, test := range []struct {
		route transfer.LaneRoute
		name  string
	}{{transfer.LaneRouteDirect, "direct"}, {transfer.LaneRouteTURN, "turn"}} {
		source.Route = test.route
		assertRoute(source, test.name)
	}
	for _, invalid := range []transfer.LaneRoute{0, transfer.LaneRouteRelay, 255} {
		source.Route = invalid
		if _, err := commandprojection.ProjectSenderAttempt(clievent.CommandShare, source); err == nil {
			t.Fatalf("invalid WebRTC admission route accepted: %d", invalid)
		}
	}
}

func TestLaneSettlementPreservesAllRouteKinds(t *testing.T) {
	session, _ := protocolsession.ProtocolSessionIDFromBytes(bytes.Repeat([]byte{1}, 16))
	for _, test := range []struct {
		route transfer.LaneRoute
		name  string
	}{{transfer.LaneRouteRelay, "relay"}, {transfer.LaneRouteDirect, "direct"}, {transfer.LaneRouteTURN, "turn"}} {
		event, err := commandprojection.ProjectLaneSettlement(transfer.LaneSettlementSummary{
			ProtocolSessionID: session, Lane: transfer.LaneIdentity{ID: 2, Epoch: 1},
			Route: test.route, DeliveredBlocks: 3, DeliveredBytes: 1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		record, err := encodeV4(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(1, 0)}, event)
		if err != nil {
			t.Fatal(err)
		}
		payload := record.Payload.(laneSettlementPayloadV4)
		if payload.Route != test.name || payload.DeliveredBlocks != "3" || payload.DeliveredBytes != "1024" {
			t.Fatalf("settlement lost path or accounting: %+v", payload)
		}
	}
}
