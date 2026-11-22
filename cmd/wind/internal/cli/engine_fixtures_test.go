package cli

import (
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/pionpeer"
)

func nativeFactSpec(side string, kind string) nativepeer.Observation {
	return nativepeer.Observation{
		Subject:  nativepeer.Subject{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, Side: side},
		Provider: &pionpeer.Event{Milestone: kind, State: "unknown"},
	}
}
