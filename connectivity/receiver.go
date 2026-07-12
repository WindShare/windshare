package connectivity

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/windshare/windshare/core/session"
)

// ChannelPool is the receiver-scheduler projection used by connectivity.
type ChannelPool interface {
	AddChannel(session.FrameChannel) error
}

type ReceiverPoolOptions struct {
	OnPeerError func(error)
}

// ReceiverPool admits relay channels immediately and negotiates at most one P2P
// channel at a time. A rejoined relay session becomes the next signaling route,
// while an already-open P2P path remains usable if the old relay disappears.
type ReceiverPool struct {
	ctx         context.Context
	cancel      context.CancelFunc
	pool        ChannelPool
	peers       OfferChannelFactory
	onPeerError func(error)

	mu               sync.Mutex
	closed           bool
	generation       uint64
	currentSignaling Signaling
	attempting       bool
	peer             PeerChannel
	peerGeneration   uint64
	peerChanges      chan struct{}
	wg               sync.WaitGroup
}

func NewReceiverPool(
	ctx context.Context,
	pool ChannelPool,
	peers OfferChannelFactory,
	options ReceiverPoolOptions,
) (*ReceiverPool, error) {
	if ctx == nil || pool == nil || peers == nil {
		return nil, fmt.Errorf("%w: context, channel pool, and offer channel factory are required", ErrNilDependency)
	}
	if options.OnPeerError == nil {
		options.OnPeerError = func(error) {}
	}
	poolCtx, cancel := context.WithCancel(ctx)
	return &ReceiverPool{
		ctx:         poolCtx,
		cancel:      cancel,
		pool:        pool,
		peers:       peers,
		onPeerError: options.OnPeerError,
		peerChanges: make(chan struct{}, 1),
	}, nil
}

// AddRelay transfers the relay channel to the scheduler, then makes its control
// lane the newest route for a future P2P attempt.
func (p *ReceiverPool) AddRelay(channel session.FrameChannel, signaling Signaling) error {
	if channel == nil || signaling == nil {
		return fmt.Errorf("%w: relay channel and signaling", ErrNilDependency)
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrReceiverPoolClosed
	}
	if err := p.ctx.Err(); err != nil {
		p.mu.Unlock()
		return err
	}
	// AddChannel is the ownership-transfer linearization point. Holding the
	// lifecycle lock prevents Close from returning before a concurrent relay is
	// either rejected or fully adopted.
	if err := p.pool.AddChannel(channel); err != nil {
		p.mu.Unlock()
		return err
	}
	if err := p.ctx.Err(); err != nil {
		p.mu.Unlock()
		_ = channel.Close()
		return err
	}
	p.setSignalingLocked(signaling)
	p.mu.Unlock()
	return nil
}

func (p *ReceiverPool) setSignalingLocked(signaling Signaling) {
	p.generation++
	p.currentSignaling = signaling
	if p.peer == nil && !p.attempting {
		p.startAttemptLocked(p.generation, signaling)
	}
}

// PeerAvailable reports whether an opened P2P channel is currently owned by
// the receiver scheduler. CLI rejoin policy uses this semantic fact instead of
// treating loss of the relay control path as loss of the established data path.
func (p *ReceiverPool) PeerAvailable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peer != nil
}

// PeerChanges is an edge-triggered wakeup; callers must re-read PeerAvailable
// after every notification because multiple transitions may coalesce.
func (p *ReceiverPool) PeerChanges() <-chan struct{} { return p.peerChanges }

func (p *ReceiverPool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	peer := p.peer
	p.notifyPeerChangeLocked()
	p.mu.Unlock()
	p.cancel()
	if peer != nil {
		// FrameChannel Close is idempotent. Closing the adopted peer here makes
		// cancellation linearizable even when factory completion races Close;
		// the scheduler may concurrently perform the same teardown.
		_ = peer.Close()
	}
	p.wg.Wait()
	return nil
}

func (p *ReceiverPool) startAttemptLocked(generation uint64, signaling Signaling) {
	p.attempting = true
	p.wg.Go(func() {
		channel, err := p.peers.Offer(p.ctx, signaling)
		if err == nil && channel == nil {
			err = fmt.Errorf("%w: offer factory returned a nil channel", ErrNilDependency)
		}
		if err == nil && channel.State() != session.Open {
			err = fmt.Errorf("%w: offer factory returned channel state %d", ErrPeerConnectionFailed, channel.State())
		}
		p.finishAttempt(generation, channel, err)
	})
}

func (p *ReceiverPool) finishAttempt(generation uint64, channel PeerChannel, err error) {
	if err != nil {
		if channel != nil {
			_ = channel.Close()
		}
		p.mu.Lock()
		p.attempting = false
		retry := !p.closed && p.ctx.Err() == nil && p.peer == nil && p.generation > generation
		nextGeneration := p.generation
		nextSignaling := p.currentSignaling
		if retry {
			p.startAttemptLocked(nextGeneration, nextSignaling)
		}
		active := !p.closed && p.ctx.Err() == nil
		p.mu.Unlock()
		if active && !isExpectedPeerEnd(err) {
			p.onPeerError(err)
		}
		return
	}

	p.mu.Lock()
	if p.closed || p.ctx.Err() != nil {
		p.attempting = false
		p.mu.Unlock()
		_ = channel.Close()
		return
	}
	if addErr := p.pool.AddChannel(channel); addErr != nil {
		p.attempting = false
		active := !p.closed && p.ctx.Err() == nil
		p.mu.Unlock()
		_ = channel.Close()
		if active && !isExpectedPeerEnd(addErr) {
			p.onPeerError(addErr)
		}
		return
	}
	if p.ctx.Err() != nil {
		p.attempting = false
		p.mu.Unlock()
		_ = channel.Close()
		return
	}
	p.attempting = false
	p.peer = channel
	p.peerGeneration = generation
	p.notifyPeerChangeLocked()
	p.wg.Go(func() { p.watchPeer(channel, generation) })
	p.mu.Unlock()
}

func (p *ReceiverPool) watchPeer(channel PeerChannel, generation uint64) {
	endedByContext := false
	select {
	case <-p.ctx.Done():
		endedByContext = true
		_ = channel.Close()
	case <-channel.Done():
	}
	peerErr := channel.Err()
	p.mu.Lock()
	if p.peer != nil && p.peerGeneration == generation {
		p.peer = nil
		p.notifyPeerChangeLocked()
	}
	if !p.closed && p.ctx.Err() == nil && !p.attempting && p.peer == nil && p.generation > generation {
		p.startAttemptLocked(p.generation, p.currentSignaling)
	}
	closed := p.closed
	p.mu.Unlock()
	if !endedByContext && !closed && !isExpectedPeerEnd(peerErr) {
		p.onPeerError(peerErr)
	}
}

func (p *ReceiverPool) notifyPeerChangeLocked() {
	select {
	case p.peerChanges <- struct{}{}:
	default:
	}
}

func isExpectedPeerEnd(err error) bool {
	return err == nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, session.ErrSessionClosed) ||
		errors.Is(err, ErrSignalingClosed) ||
		errors.Is(err, ErrReceiverPoolClosed)
}
