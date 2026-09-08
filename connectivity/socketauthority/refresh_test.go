package socketauthority

import (
	"context"
	"testing"
	"time"

	"github.com/pion/ice/v4"
)

func TestAlreadyCanceledRefreshDoesNotTouchSocket(t *testing.T) {
	socket := newIdleTestSocket(false)
	mux := ice.NewUniversalUDPMuxDefault(ice.UniversalUDPMuxParams{UDPConn: socket})
	t.Cleanup(func() { _ = mux.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mux.RefreshXORMappedAddr(ctx, socket.LocalAddr(), time.Minute); err != context.Canceled {
		t.Fatalf("refresh ignored caller cancellation: %v", err)
	}
	select {
	case <-socket.written:
		t.Fatal("already canceled refresh sent a packet")
	default:
	}
}
