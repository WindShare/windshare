package relayv2

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/internal/connectionliveness"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

var nextLinkID atomic.Uint64

type link struct {
	id        uint64
	socket    BinarySocket
	ctx       context.Context
	cancel    context.CancelFunc
	heartbeat HeartbeatConfig
	liveness  *connectionliveness.Monitor
	probes    probeLedger

	writeWake chan struct{}
	writeMu   sync.Mutex
	queues    map[v2.RelaySessionID]*sendQueue
	order     []v2.RelaySessionID
	cursor    int
	queued    int
	windows   map[v2.RelaySessionID]*sendWindow
	receives  *receiveBuffer
	// Observation completion joins only accepted sends that can still publish a
	// result fact; disabled tracing pays no registration or synchronization cost.
	sendResultMu         sync.Mutex
	pendingSendResults   map[*sendRequest]struct{}
	sendResultPublishers sync.WaitGroup

	channelMu sync.Mutex
	channels  map[v2.RelaySessionID]*Channel
	accept    chan *Channel
	fixed     bool

	lifecycleMu  sync.Mutex
	lifecycle    linkLifecycle
	retirement   *retirementRecord
	shutdownOnce sync.Once
	done         chan struct{}

	traces        *lifecycleSource
	nextOperation atomic.Uint64
}

func newLink(parent context.Context, socket BinarySocket, fixed bool, options DialOptions) (*link, error) {
	capacity, err := normalizeReceiveCapacity(options.ReceiveCapacity)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	linkID := nextLinkID.Add(1)
	l := &link{
		id: linkID, socket: socket, ctx: ctx, cancel: cancel, fixed: fixed,
		heartbeat: options.Heartbeat,
		liveness:  connectionliveness.NewMonitor(time.Now()),
		writeWake: make(chan struct{}, 1), queues: make(map[v2.RelaySessionID]*sendQueue),
		windows:  make(map[v2.RelaySessionID]*sendWindow),
		channels: make(map[v2.RelaySessionID]*Channel), accept: make(chan *Channel, channelReceiveFrames),
		done: make(chan struct{}), traces: newLifecycleSource(linkID, options.LifecycleObservationCapacity),
	}
	l.receives, err = newReceiveBuffer(linkID, capacity, l.wakeWriter, l.stop)
	if err != nil {
		cancel()
		return nil, err
	}
	return l, nil
}

func (l *link) wakeWriter() {
	select {
	case l.writeWake <- struct{}{}:
	default:
	}
}

func (l *link) nextOperationID() uint64 {
	return l.nextOperation.Add(1)
}

func (l *link) trace(event LifecycleTrace) {
	if l.traces != nil {
		event.LinkID = l.id
		l.traces.emit(event)
	}
}

func (l *link) lifecycleTrace() <-chan LifecycleTrace {
	if l == nil {
		return nil
	}
	return l.traces.stream()
}

func (l *link) completeObservations() LifecycleObservationCompletion {
	if l == nil {
		return LifecycleObservationCompletion{}
	}
	return l.traces.complete()
}

func (l *link) completeObservationsWithFinal(event LifecycleTrace) LifecycleObservationCompletion {
	if l == nil {
		return LifecycleObservationCompletion{}
	}
	event.LinkID = l.id
	return l.traces.completeWithFinal(event)
}

func (l *link) traceTransition(channel *Channel, transition retirementTransition) {
	if !transition.applied && !transition.deferred {
		return
	}
	if transition.deferred {
		l.trace(retirementDeferredTrace(
			channel.id,
			transition.record.operationID,
			transition.terminal,
			transition.record.source,
			lifecycleCause(transition.record.cause),
			lifecycleCause(transition.consequence.cause),
		))
		return
	}
	l.trace(retiredTrace(
		channel.id,
		transition.record.operationID,
		transition.terminal,
		transition.record.source,
		lifecycleCause(transition.record.cause),
		lifecycleCause(transition.consequence.cause),
	))
}

func (l *link) start() {
	heartbeat, err := l.heartbeat.Normalize()
	if err != nil {
		// Dialers validate the configuration; fail closed for direct owners.
		l.stop(err)
		return
	}
	l.heartbeat = heartbeat
	go l.readLoop()
	go l.writeLoop()
	go l.heartbeatLoop()
}

func (l *link) readLoop() {
	for {
		messageType, encoded, err := l.socket.Read(l.ctx)
		if err != nil {
			l.stop(err)
			return
		}
		if err := l.receive(messageType, encoded); err != nil {
			l.stop(err)
			return
		}
		// Every accepted frame proves the relay alive, so liveness never waits
		// on an acknowledgement queued behind content.
		l.liveness.Observe(time.Now())
	}
}

func (l *link) receive(messageType websocket.MessageType, encoded []byte) error {
	if messageType != websocket.MessageBinary || len(encoded) < 4 {
		return ErrProtocol
	}
	switch string(encoded[:4]) {
	case v2.SettlementNoticeMagic:
		notice, err := v2.ParseSettlementNotice(encoded)
		if err != nil {
			return ErrProtocol
		}
		l.retire(notice.RelaySessionID)
		if err := l.receives.notice(notice.RelaySessionID, notice); err != nil {
			return errors.Join(ErrProtocol, err)
		}
		return nil
	case v2.SessionCreditMagic:
		credit, err := v2.ParseSessionCredit(encoded)
		if err != nil || !l.replenishCredit(credit) {
			return ErrProtocol
		}
		return nil
	case v2.ConnectionProbeAckMagic:
		return l.acknowledgeProbe(encoded)
	}
	route, err := v2.ParseOpaqueRoute(encoded)
	if err != nil {
		return ErrProtocol
	}
	l.channelMu.Lock()
	channel := l.channels[route.RelaySessionID]
	l.channelMu.Unlock()
	if channel == nil {
		return ErrProtocol
	}
	if err := channel.deliver(route.Ciphertext); err != nil {
		return errors.Join(ErrProtocol, err)
	}
	if !l.fixed && !channel.announced && channel.State() == framechannel.Open {
		channel.announced = true
		select {
		case l.accept <- channel:
		default:
			l.failChannel(channel, LifecycleRetirementIngressFailure, ErrIngressOverflow)
			return nil
		}
	}
	return nil
}

func (l *link) writeLoop() {
	flush := time.NewTimer(receiveFlushInterval)
	flush.Stop()
	defer flush.Stop()
	for {
		// Probes bypass credit waits: a blocked content window must not keep
		// the relay from seeing this client alive.
		if probe, ok := l.takeProbe(); ok {
			if err := l.socket.Write(l.ctx, websocket.MessageBinary, probe); err != nil {
				l.stop(err)
				return
			}
			continue
		}
		encoded, settled, err := l.receives.take(time.Now())
		if err != nil {
			l.stop(err)
			return
		}
		if encoded != nil {
			if err := l.socket.Write(l.ctx, websocket.MessageBinary, encoded); err != nil {
				l.stop(err)
				return
			}
			if settled != (v2.RelaySessionID{}) {
				l.channelMu.Lock()
				delete(l.channels, settled)
				l.channelMu.Unlock()
			}
			continue
		}
		request, ok := l.takeRequest()
		if ok {
			err := l.socket.Write(l.ctx, websocket.MessageBinary, request.data)
			l.resolveRequest(request, err, true)
			if err != nil {
				l.stop(err)
				return
			}
			continue
		}
		var flushDue <-chan time.Time
		if deadline := l.receives.deadline(); !deadline.IsZero() {
			flush.Reset(time.Until(deadline))
			flushDue = flush.C
		}
		select {
		case <-l.ctx.Done():
			return
		case <-l.writeWake:
		case <-flushDue:
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
	if channel == nil {
		return nil, false
	}
	l.channels[id] = channel
	return channel, true
}

func (l *link) installFixed(id v2.RelaySessionID) *Channel {
	l.lifecycleMu.Lock()
	defer l.lifecycleMu.Unlock()
	l.channelMu.Lock()
	defer l.channelMu.Unlock()
	if l.lifecycle != linkOpen {
		return nil
	}
	channel := newChannel(id, l)
	if channel != nil {
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
	l.requestChannelRetirement(channel, record)
}

func (l *link) failChannel(channel *Channel, source LifecycleRetirementSource, cause error) {
	record := retirementRecord{
		source: source, cause: cause, operationID: l.nextOperationID(),
	}
	l.requestChannelRetirement(channel, record)
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
		// Accepted terminal frames can outlive a network failure. A later owner
		// close must still join their workers and release the remaining storage.
		if cause == nil {
			l.receives.close(true)
		}
		return
	}
	first := l.lifecycle == linkOpen
	if first {
		l.lifecycle = linkRetiring
		record := proposed
		l.retirement = &record
	}
	authoritative := *l.retirement
	l.trace(linkRetiringTrace(
		proposed.operationID,
		authoritative.source,
		lifecycleCause(authoritative.cause),
		lifecycleCause(proposed.cause),
	))
	l.lifecycleMu.Unlock()
	channels := l.snapshotChannels()
	waiters := make([]<-chan struct{}, 0, len(channels))
	if first || cause != nil {
		for _, channel := range channels {
			done := l.requestChannelRetirement(channel, proposed)
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
	// Failure retirement can cancel an in-flight write that was already removed
	// from the queue. Keep the stream open until that accepted result publishes.
	l.receives.close(cause == nil)
	l.cancel()
	if l.traces != nil {
		if authoritative.cause != nil {
			l.resolvePendingRequests(authoritative.sendError())
		}
		l.sendResultPublishers.Wait()
	}
	l.shutdownOnce.Do(func() {
		_ = l.socket.Close(websocket.StatusNormalClosure, "")
		l.lifecycleMu.Lock()
		l.lifecycle = linkClosed
		l.lifecycleMu.Unlock()
		l.completeObservationsWithFinal(linkClosedTrace(
			authoritative.operationID,
			authoritative.source,
			lifecycleCause(authoritative.cause),
		))
		// Done follows the producer cut so observers never see transport completion
		// before the terminal link fact has been admitted and the stream sealed.
		close(l.done)
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
