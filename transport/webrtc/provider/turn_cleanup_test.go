package provider

import (
	"net"
	"testing"
	"time"

	"github.com/pion/turn/v5"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

func TestTURNCloseCancelsPendingAllocationTransaction(t *testing.T) {
	server, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	authority := socketauthority.New(socketauthority.Config{})
	t.Cleanup(func() { _ = authority.Close() })
	connection := turnConnection(t, testLease(t, authority, 1, "127.0.0.1"),
		"turn:"+server.LocalAddr().String()+"?transport=udp")
	if _, err = connection.CreateDataChannel("cancel-allocation", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := connection.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = connection.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	// After a retransmission the next retry is beyond the close grace. Waiting
	// for that retry would conceal a stranded allocation transaction.
	if err = server.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, err = server.ReadFrom(make([]byte, 2048)); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- connection.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending TURN transaction blocked Close")
	}
	if resources, _ := connection.TURNResources(); resources.ActiveSockets != 0 {
		t.Fatal(resources)
	}
}

func awaitTURNAllocations(t *testing.T, server *turn.Server, count int) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for server.AllocationCount() != count {
		select {
		case <-deadline.C:
			t.Fatalf("server allocations = %d, want %d", server.AllocationCount(), count)
		case <-tick.C:
		}
	}
}
