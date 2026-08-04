package main

import (
	"net"
	"testing"
)

func TestReadyRecordPublishesActualOwnedListener(t *testing.T) {
	record, err := newReadyRecord(
		&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 34_781},
		net.IPv4(192, 0, 2, 10),
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.Component != "browser-local-turn-server" ||
		record.ScenarioID != "chromium-turn-route" ||
		record.OperationID != "chromium-turn-route-server" ||
		record.Milestone != "listener-ready" ||
		record.URL != "turn:127.0.0.1:34781?transport=udp" || record.RelayAddress != "192.0.2.10" ||
		record.Username != username || record.Credential != credential {
		t.Fatalf("ready record = %+v", record)
	}
}

func TestReadyRecordRejectsNonLoopbackListener(t *testing.T) {
	relayIP := net.IPv4(192, 0, 2, 10)
	if _, err := newReadyRecord(&net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 34_781}, relayIP); err == nil {
		t.Fatal("ready record accepted a non-loopback TURN listener")
	}
	if _, err := newReadyRecord(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 34_781}, relayIP); err == nil {
		t.Fatal("ready record accepted a non-UDP listener")
	}
	if _, err := newReadyRecord(&net.UDPAddr{IP: net.IPv6loopback, Port: 34_781}, relayIP); err == nil {
		t.Fatal("ready record accepted an IPv6 listener for its IPv4 endpoint")
	}
}

func TestReadyRecordRejectsUnusableRelayAddress(t *testing.T) {
	listener := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 34_781}
	for _, relayIP := range []net.IP{nil, net.IPv4zero, net.IPv4(127, 0, 0, 1), net.IPv6loopback} {
		if _, err := newReadyRecord(listener, relayIP); err == nil {
			t.Fatalf("ready record accepted relay IP %v", relayIP)
		}
	}
}
