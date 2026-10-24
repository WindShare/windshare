package cli

import (
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
