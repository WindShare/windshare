package relayv2

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

var nextLinkID atomic.Uint64

type link struct {
	id     uint64
	socket BinarySocket
	ctx    context.Context
	cancel context.CancelFunc

	writeWake chan struct{}
	writeMu   sync.Mutex
	queues    map[v2.RelaySessionID]*sendQueue
	order     []v2.RelaySessionID
	cursor    int
	queued    int

	channelMu sync.Mutex
	channels  map[v2.RelaySessionID]*Channel
	accept    chan *Channel
	fixed     bool

	lifecycleMu  sync.Mutex
	lifecycle    linkLifecycle
	retirement   *retirementRecord
	shutdownOnce sync.Once
	done         chan struct{}

	tracer        LifecycleTracer
	nextOperation atomic.Uint64
}

func newLink(parent context.Context, socket BinarySocket, fixed bool) *link {
	return newLinkWithTracer(parent, socket, fixed, nil)
}

func newLinkWithTracer(parent context.Context, socket BinarySocket, fixed bool, tracer LifecycleTracer) *link {
	ctx, cancel := context.WithCancel(parent)
	return &link{
		id: nextLinkID.Add(1), socket: socket, ctx: ctx, cancel: cancel, fixed: fixed,
		writeWake: make(chan struct{}, 1), queues: make(map[v2.RelaySessionID]*sendQueue),
		channels: make(map[v2.RelaySessionID]*Channel), accept: make(chan *Channel, channelReceiveFrames),
		done: make(chan struct{}), tracer: tracer,
	}
}

func (l *link) nextOperationID() uint64 {
	return l.nextOperation.Add(1)
}

func (l *link) trace(event LifecycleTrace) {
	if l.tracer != nil {
		event.LinkID = l.id
		l.tracer.TraceRelayLifecycle(event)
	}
}

func (l *link) traceTransition(channel *Channel, transition retirementTransition) {
	if !transition.applied && !transition.deferred {
		return
	}
	stage := LifecycleRetired
	if transition.deferred {
		stage = LifecycleRetirementDeferred
	}
	l.trace(LifecycleTrace{
		RelaySessionID: channel.id, OperationID: transition.record.operationID,
		Stage: stage, Terminal: transition.terminal,
		RetirementSource: transition.record.source,
		Cause:            lifecycleCause(transition.record.cause),
		DrainCause:       lifecycleCause(transition.consequence.cause),
	})
}

func (l *link) start() {
	go l.readLoop()
	go l.writeLoop()
}

func (l *link) readLoop() {
	for {
		messageType, encoded, err := l.socket.Read(l.ctx)
		if err != nil {
			l.stop(err)
			return
		}
		if messageType != websocket.MessageBinary || len(encoded) < 4 {
			l.stop(ErrProtocol)
			return
		}
		if string(encoded[:4]) == v2.SessionRetiredMagic {
			retired, parseErr := v2.ParseSessionRetired(encoded)
			if parseErr != nil {
				l.stop(ErrProtocol)
				return
			}
			l.retire(retired.RelaySessionID)
			continue
		}
		route, parseErr := v2.ParseOpaqueRoute(encoded)
		if parseErr != nil {
			l.stop(ErrProtocol)
			return
		}
		channel, created := l.channel(route.RelaySessionID)
		if channel == nil {
			l.stop(ErrProtocol)
			return
		}
		if created {
			select {
			case l.accept <- channel:
			default:
				l.failChannel(channel, LifecycleRetirementIngressFailure, ErrIngressOverflow)
				continue
			}
		}
		if !channel.deliver(route.Ciphertext) {
			l.failChannel(channel, LifecycleRetirementIngressFailure, ErrIngressOverflow)
		}
	}
}

func (l *link) writeLoop() {
	for {
		request, ok := l.takeRequest()
		if ok {
			err := l.socket.Write(l.ctx, websocket.MessageBinary, request.data)
			request.receipt <- err
			if err != nil {
				l.stop(err)
				return
			}
			continue
		}
		select {
		case <-l.ctx.Done():
			return
		case <-l.writeWake:
		}
	}
}

func (l *link) channel(id v2.RelaySessionID) (*Channel, bool) {
	l.lifecycleMu.Lock()
	defer l.lifecycleMu.Unlock()
	if l.lifecycle != linkOpen {
		return nil, false
	}
	l.channelMu.Lock()
	defer l.channelMu.Unlock()
	if existing := l.channels[id]; existing != nil {
		return existing, false
	}
	if l.fixed {
		return nil, false
	}
	channel := newChannel(id, l)
	l.channels[id] = channel
	return channel, true
}

func (l *link) installFixed(id v2.RelaySessionID) *Channel {
	l.lifecycleMu.Lock()
	defer l.lifecycleMu.Unlock()
	l.channelMu.Lock()
	defer l.channelMu.Unlock()
	channel := newChannel(id, l)
	if l.lifecycle == linkOpen {
		l.channels[id] = channel
	}
	return channel
}

func (l *link) retire(id v2.RelaySessionID) {
	l.channelMu.Lock()
	channel := l.channels[id]
	l.channelMu.Unlock()
	if channel == nil {
		return
	}
	record := retirementRecord{
		natural: true, source: LifecycleRetirementRelaySession,
		cause: ErrSessionRetired, operationID: l.nextOperationID(),
	}
	_, transition := l.requestChannelRetirement(channel, record)
	l.traceTransition(channel, transition)
}

func (l *link) failChannel(channel *Channel, source LifecycleRetirementSource, cause error) {
	record := retirementRecord{
		source: source, cause: cause, operationID: l.nextOperationID(),
	}
	_, transition := l.requestChannelRetirement(channel, record)
	l.traceTransition(channel, transition)
}

func (l *link) stop(cause error) {
	proposed := retirementRecord{
		natural: cause == nil, source: LifecycleRetirementLinkFailure,
		cause: cause, operationID: l.nextOperationID(),
	}
	if proposed.natural {
		proposed.source = LifecycleRetirementLinkClose
	}

	l.lifecycleMu.Lock()
	if l.lifecycle == linkClosed {
		l.lifecycleMu.Unlock()
		return
	}
	first := l.lifecycle == linkOpen
	if first {
		l.lifecycle = linkRetiring
		record := proposed
		l.retirement = &record
	}
	authoritative := *l.retirement
	l.lifecycleMu.Unlock()

	l.trace(LifecycleTrace{
		OperationID: proposed.operationID, Stage: LifecycleLinkRetiring,
		RetirementSource: authoritative.source, Cause: lifecycleCause(authoritative.cause),
		DrainCause: lifecycleCause(proposed.cause),
	})
	channels := l.snapshotChannels()
	waiters := make([]<-chan struct{}, 0, len(channels))
	if first || cause != nil {
		for _, channel := range channels {
			done, transition := l.requestChannelRetirement(channel, proposed)
			l.traceTransition(channel, transition)
			if first && authoritative.natural && done != nil {
				waiters = append(waiters, done)
			}
		}
	}
	if !first {
		return
	}
	if authoritative.natural {
		for _, done := range waiters {
			<-done
		}
	}
	l.shutdownOnce.Do(func() {
		l.cancel()
		_ = l.socket.Close(websocket.StatusNormalClosure, "")
		l.lifecycleMu.Lock()
		l.lifecycle = linkClosed
		l.lifecycleMu.Unlock()
		close(l.done)
		l.trace(LifecycleTrace{
			OperationID: authoritative.operationID, Stage: LifecycleLinkClosed,
			RetirementSource: authoritative.source, Cause: lifecycleCause(authoritative.cause),
		})
	})
}

func (l *link) snapshotChannels() []*Channel {
	l.channelMu.Lock()
	defer l.channelMu.Unlock()
	channels := make([]*Channel, 0, len(l.channels))
	for _, channel := range l.channels {
		channels = append(channels, channel)
	}
	return channels
}

func (l *link) Err() error {
	l.lifecycleMu.Lock()
	defer l.lifecycleMu.Unlock()
	if l.retirement == nil {
		return nil
	}
	return l.retirement.cause
}
