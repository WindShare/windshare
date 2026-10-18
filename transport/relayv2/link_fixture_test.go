package relayv2

import (
	"context"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func newTestLink(ctx context.Context, socket BinarySocket, fixed bool) *link {
	return newTestLinkWithLifecycleStream(ctx, socket, fixed, 0)
}

func newTestLinkWithLifecycleStream(ctx context.Context, socket BinarySocket, fixed bool, capacity int) *link {
	l, err := newLink(ctx, socket, fixed, DialOptions{LifecycleObservationCapacity: capacity})
	if err != nil {
		panic(err)
	}
	return l
}

// Lifecycle tests start after admission; install the same explicit grant that a
// relay supplies. Window-admission tests exercise the ungranted production path.
func installGrantedChannel(l *link, id v2.RelaySessionID) *Channel {
	channel := l.installFixed(id)
	if !l.replenishCredit(v2.SessionCredit{RelaySessionID: id, Frames: v2.SenderWindowFrames, Bytes: v2.SenderWindowBytes}) {
		panic("test grant rejected")
	}
	return channel
}
