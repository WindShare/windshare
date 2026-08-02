package relayv2

import (
	"bytes"
	"context"
	"testing"

	"github.com/coder/websocket"
)

func TestRelaySenderRegistrationWireContract(t *testing.T) {
	fixture, err := newRegistrationContractFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := runSenderRegistrationContract(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if stats.BytesSent == 0 || stats.BytesReceived == 0 {
		t.Fatalf("registration stats are empty: %+v", stats)
	}
}

func TestRegistrationSocketRejectsCrossDirectionReordering(t *testing.T) {
	fixture, err := newRegistrationContractFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("challenge before init", func(t *testing.T) {
		socket := newRegistrationContractSocket(fixture)
		t.Cleanup(func() { _ = socket.Close(websocket.StatusNormalClosure, "") })
		if _, _, err := socket.Read(context.Background()); err == nil ||
			!bytes.Contains([]byte(err.Error()), []byte("want REGISTER_INIT write")) {
			t.Fatalf("out-of-order challenge error = %v", err)
		}
	})
	t.Run("proof before challenge", func(t *testing.T) {
		socket := newRegistrationContractSocket(fixture)
		t.Cleanup(func() { _ = socket.Close(websocket.StatusNormalClosure, "") })
		if err := socket.Write(context.Background(), websocket.MessageBinary, fixture.transcript[0].encoded); err != nil {
			t.Fatal(err)
		}
		if err := socket.Write(context.Background(), websocket.MessageBinary, fixture.transcript[2].encoded); err == nil ||
			!bytes.Contains([]byte(err.Error()), []byte("want CHALLENGE read")) {
			t.Fatalf("out-of-order proof error = %v", err)
		}
	})
	t.Run("registered before upload", func(t *testing.T) {
		socket := newRegistrationContractSocket(fixture)
		t.Cleanup(func() { _ = socket.Close(websocket.StatusNormalClosure, "") })
		if err := socket.Write(context.Background(), websocket.MessageBinary, fixture.transcript[0].encoded); err != nil {
			t.Fatal(err)
		}
		if _, _, err := socket.Read(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := socket.Write(context.Background(), websocket.MessageBinary, fixture.transcript[2].encoded); err != nil {
			t.Fatal(err)
		}
		if _, _, err := socket.Read(context.Background()); err == nil ||
			!bytes.Contains([]byte(err.Error()), []byte("want DESCRIPTOR_UPLOAD write")) {
			t.Fatalf("out-of-order registered error = %v", err)
		}
	})
}
