package cli

import (
	"testing"

	appobs "github.com/windshare/windshare/engine/observation"
)

func testShareBytes16(marker byte) []byte { data := make([]byte, 16); data[0] = marker; return data }
func nativeFactSpec(workflow appobs.Workflow, kind string) appobs.NativeConnectivitySpec {
	session, _ := appobs.NewProtocolSessionID(testShareBytes16(1))
	path, _ := appobs.NewPeerPathID(testShareBytes16(2))
	side := "sender"
	if workflow == appobs.WorkflowReceive {
		side = "receiver"
	}
	return appobs.NativeConnectivitySpec{Workflow: workflow, Session: session, Path: path, Side: side, Kind: kind, State: "unknown"}
}
func peerFact(t *testing.T, stage appobs.PeerAttemptStage, route appobs.LaneRoute) appobs.PeerAttemptObserved {
	t.Helper()
	session, _ := appobs.NewProtocolSessionID(testShareBytes16(1))
	path, _ := appobs.NewPeerPathID(testShareBytes16(2))
	attempt, _ := appobs.NewPeerAttemptID(testShareBytes16(3))
	grant, _ := appobs.NewProtocolOperationID(testShareBytes16(4))
	lane, _ := appobs.NewLaneIdentity(2, 1)
	spec := appobs.PeerAttemptSpec{Workflow: appobs.WorkflowShare, Session: session, PeerPath: path, Attempt: attempt, Sequence: 1, Stage: stage, Route: route, Phase: appobs.PeerPhaseAdmission, GrantOperation: grant, HasGrantOperation: true, Lane: lane, HasLane: true}
	if stage == appobs.PeerAttemptAdmitted {
		spec.AdmissionDisposition = appobs.PeerAdmissionAccepted
		spec.ResponseDelivery = appobs.PeerResponseDelivered
	}
	value, err := appobs.NewPeerAttemptObserved(spec)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
