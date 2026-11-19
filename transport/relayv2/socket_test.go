package relayv2

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/core/framechannel"
)

func TestQueuedWriteAndLinkReportSameSocketFailure(t *testing.T) {
	const closeReason = "remote close reason"
	cause := websocket.CloseError{Code: websocket.StatusNormalClosure, Reason: closeReason}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	link := newTestLink(ctx, writeFailureSocket{BinarySocket: newScriptedSocket(), cause: cause}, false)
	t.Cleanup(func() { link.stop(nil) })
	channel := installGrantedChannel(link, relaySessionID(1))
	go link.writeLoop()

	sendErr := channel.Send(ctx, framechannel.Frame("outgoing"))
	select {
	case <-link.done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !errors.Is(sendErr, ErrClosed) || !errors.Is(sendErr, cause) || strings.Contains(sendErr.Error(), closeReason) {
		t.Fatalf("send returned %v, want a safe closed-socket error with the original cause", sendErr)
	}
	if linkErr := link.Err(); sendErr != linkErr {
		t.Fatalf("send error = %v, link error = %v, want same failure", sendErr, linkErr)
	}
}
