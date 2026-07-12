package connectivity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	pion "github.com/pion/webrtc/v4"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

type dataChannelResult struct {
	channel *ownedPeerChannel
	err     error
}

// ownedPeerChannel binds the transport channel's explicit Close to its parent
// PeerConnection. The transport adapter cannot own that parent without also
// absorbing signaling policy, so connectivity supplies the missing ownership
// edge while preserving the adapter's terminal-drain semantics.
type ownedPeerChannel struct {
	PeerChannel
	releaseOnce sync.Once
	release     context.CancelFunc
	released    atomic.Bool

	errMu    sync.Mutex
	ownerErr error
}

func (c *ownedPeerChannel) Close() error {
	err := c.PeerChannel.Close()
	// A completed owner Close is the graceful cancellation source. Recording it
	// before canceling lets maintenance distinguish that path from cancellation
	// that must abort a terminal-pending peer connection.
	c.released.Store(true)
	c.releaseOnce.Do(c.release)
	return err
}

func (c *ownedPeerChannel) Err() error {
	c.errMu.Lock()
	ownerErr := c.ownerErr
	c.errMu.Unlock()
	channelErr := c.PeerChannel.Err()
	if ownerErr == nil {
		return channelErr
	}
	if channelErr == nil {
		return ownerErr
	}
	return errors.Join(ownerErr, channelErr)
}

func (c *ownedPeerChannel) fail(err error) {
	if err == nil {
		return
	}
	c.errMu.Lock()
	if c.ownerErr == nil {
		c.ownerErr = err
	}
	c.errMu.Unlock()
}

func (c *ownedPeerChannel) releasedByOwner() bool { return c.released.Load() }

func (n *pionNegotiation) own(channel PeerChannel) *ownedPeerChannel {
	return &ownedPeerChannel{PeerChannel: channel, release: n.cancel}
}

func (n *pionNegotiation) configureRemoteDataChannels(
	role negotiationRole,
	results chan<- dataChannelResult,
) {
	n.peer.OnDataChannel(func(dataChannel *pion.DataChannel) {
		if role != negotiationAnswerer || !n.remoteDataChannels.CompareAndSwap(0, 1) {
			_ = dataChannel.Close()
			n.report(negotiationFailure{
				err:            ErrUnexpectedDataChannel,
				fatalAfterOpen: true,
			})
			return
		}
		wrapped, err := transportwebrtc.NewChannel(dataChannel)
		if err != nil {
			_ = dataChannel.Close()
		}
		result := dataChannelResult{err: err}
		if wrapped != nil {
			result.channel = n.own(wrapped)
		}
		select {
		case <-n.ctx.Done():
			if result.channel != nil {
				go func() { _ = result.channel.Close() }()
			}
		case results <- result:
		}
	})
}
