package cli

import (
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/pionpeer"
	appobs "github.com/windshare/windshare/engine/observation"
)

func nativeFactSpec(workflow appobs.Workflow, kind string) nativepeer.Observation {
	side := nativepeer.SideSender
	if workflow == appobs.WorkflowReceive {
		side = nativepeer.SideReceiver
	}
	return nativepeer.Observation{Subject: nativepeer.Subject{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, Side: side}, Provider: &pionpeer.Event{Milestone: kind, State: "unknown"}}
}
