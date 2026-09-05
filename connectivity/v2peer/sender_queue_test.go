package v2peer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

type handshakeClock struct {
	mu    sync.Mutex
	now   time.Time
	tasks []*handshakeTask
}
type handshakeTask struct {
	at      time.Time
	run     func()
	stopped atomic.Bool
}

func (c *handshakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *handshakeClock) AfterFunc(d time.Duration, f func()) func() {
	c.mu.Lock()
	task := &handshakeTask{at: c.now.Add(d), run: f}
	c.tasks = append(c.tasks, task)
	c.mu.Unlock()
	return func() { task.stopped.Store(true) }
}
func (c *handshakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*handshakeTask
	for _, task := range c.tasks {
		if !task.at.After(c.now) && !task.stopped.Swap(true) {
			due = append(due, task)
		}
	}
	c.mu.Unlock()
	for _, task := range due {
		task.run()
	}
}

type handshakeTimers struct {
	clock   *handshakeClock
	created chan recordedReceiverPhaseTimer
}

func (s handshakeTimers) NewPeerPhaseTimer(phase PeerAttemptPhase, d time.Duration) (PeerPhaseTimer, error) {
	timer := newReceiverManualTimer()
	s.clock.AfterFunc(d, timer.Fire)
	s.created <- recordedReceiverPhaseTimer{phase: phase, duration: d, timer: timer}
	return timer, nil
}

type handshakeOperation struct {
	*receiverTestOperation
	sender *peerAttempt
}

func (o *handshakeOperation) Terminate(ctx context.Context) ReceiverSignalingTermination {
	terminal := o.receiverTestOperation.Terminate(ctx)
	_ = o.sender.cancelOperation(ctx)
	return terminal
}

type queuedHandshake struct {
	clock                        *handshakeClock
	senderTimers, receiverTimers handshakeTimers
	sender                       *peerAttempt
	receiver                     *ReceiverAttempt
	native                       *nativepeer.NativePeerConnectivity
	blockers                     []*provider.Connection
	operation                    *handshakeOperation
	session                      *testPeerSession
	started                      *atomic.Int32
}

func newQueuedHandshake(t *testing.T) *queuedHandshake {
	t.Helper()
	clock := &handshakeClock{now: time.Unix(1, 0)}
	makeTimers := func() handshakeTimers {
		return handshakeTimers{clock: clock, created: make(chan recordedReceiverPhaseTimer, 8)}
	}
	h := &queuedHandshake{clock: clock, senderTimers: makeTimers(), receiverTimers: makeTimers(), session: newTestPeerSession(90), started: &atomic.Int32{}}
	newNative := func(count *atomic.Int32) *nativepeer.NativePeerConnectivity {
		pool, _ := icepolicy.NewICEEndpointPool(nil)
		gate := nativepeer.NewProcessAdmission(nativepeer.AdmissionClock{Now: clock.Now, AfterFunc: clock.AfterFunc})
		native := nativepeer.New(nativepeer.Config{Admission: gate, Pool: &pool, Monitor: networkstate.NewMonitor(processPhaseNetwork{}, time.Nanosecond), Reachability: reachability.New(reachability.Config{}), Now: clock.Now, ObservationCapacity: 128, Connect: func(config pion.Configuration, request provider.AttemptConfig) (*provider.Connection, error) {
			if count != nil {
				count.Add(1)
			}
			return provider.NewPeerConnection(config, request)
		}})
		t.Cleanup(func() { _ = native.Close(context.Background()) })
		return native
	}
	h.native = newNative(h.started)
	for i := byte(1); i <= nativepeer.ProcessConcurrentAttempts; i++ {
		connection, err := h.native.NewPeerConnection(context.Background(), nativepeer.AttemptRequest{ProtocolSessionID: [16]byte{i}, Binding: testBinding(i)})
		if err != nil {
			t.Fatal(err)
		}
		h.blockers = append(h.blockers, connection)
	}
	senderFactory, err := NewFactory(Config{Native: h.native, PhaseTimers: h.senderTimers, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	receiverFactory, err := NewReceiverFactory(ReceiverFactoryConfig{Native: newNative(nil), PhaseTimers: h.receiverTimers})
	if err != nil {
		t.Fatal(err)
	}
	h.operation = &handshakeOperation{receiverTestOperation: newReceiverTestOperation()}
	signaling := &receiverTestSignaling{offers: make(chan []byte, 2), open: func(_ context.Context, binding ReceiverSignalingOperationBinding, body []byte) (ReceiverSignalingOperation, error) {
		offer, decodeErr := v2signal.DecodeOffer(body)
		if decodeErr != nil {
			return nil, decodeErr
		}
		h.sender = newPeerAttempt(peerAttemptConfig{factory: senderFactory, session: h.session, operation: h.operation.id, offer: offer, onDone: func(*peerAttempt, error) {}})
		h.operation.sender = h.sender
		h.operation.bindReceiverSignalingOperation(binding)
		var work sync.WaitGroup
		work.Add(1)
		h.sender.start(context.Background(), &work)
		return h.operation, nil
	}}
	h.receiver, err = receiverFactory.StartBinding(context.Background(), signaling, processPhaseLanes{newReceiverTestLanes()}, testBinding(99))
	if err != nil {
		t.Fatal(err)
	}
	// Queue observation is the synchronization boundary after real offer creation
	// and receiver-to-sender signaling; the sender has not built a provider yet.
	for {
		event := receiveTest(t, h.native.Observations())
		if event.Admission != nil && event.Admission.Kind == nativepeer.AdmissionQueued && event.Subject.ProtocolSessionID == [16]byte(h.session.sessionID) {
			break
		}
	}
	t.Cleanup(func() { h.sender.stop(context.Canceled); _ = h.receiver.Close(); receiveTest(t, h.sender.done) })
	return h
}
func TestSenderCapacityWaitSharesPreparationAndBothPeersKeepFullChecking(t *testing.T) {
	h := newQueuedHandshake(t)
	receiverPrep := receiveTest(t, h.receiverTimers.created)
	var senderPrep recordedReceiverPhaseTimer
	select {
	case senderPrep = <-h.senderTimers.created:
	default:
		t.Fatal("sender queue is outside signaling preparation")
	}
	if senderPrep.phase != PeerAttemptPhasePreparation || receiverPrep.duration != PeerSignalingPreparationBudget {
		t.Fatal(senderPrep, receiverPrep)
	}
	h.clock.Advance(9 * time.Second)
	_ = h.blockers[0].Close()
	var answer capturedControl
	for {
		answer = receiveTest(t, h.session.controls)
		if answer.kind == protocolsession.MessagePeerAnswer {
			break
		}
	}
	h.operation.controls <- receiverTestControl{kind: answer.kind, body: answer.body}
	for _, source := range []handshakeTimers{h.senderTimers, h.receiverTimers} {
		checking := receiveTest(t, source.created)
		if checking.phase != PeerAttemptPhaseChecking || checking.duration != PeerICECheckingBudget {
			t.Fatalf("checking got reset preparation or shortened: %+v", checking)
		}
	}
	h.clock.Advance(39 * time.Second)
	if h.receiver.phaseContext.Err() != nil {
		t.Fatal("receiver lost full checking window", h.receiver.phaseContext.Err())
	}
	h.sender.phases.mu.Lock()
	stage := h.sender.phases.stage
	pending := h.sender.phases.pendingExpiration
	h.sender.phases.mu.Unlock()
	if stage != PeerAttemptPhaseChecking || pending.generation != 0 {
		t.Fatal("sender lost full checking window", stage, pending)
	}
	// The retired original preparation timers have fired at fake t=48s without
	// cancelling the checking phase whose real start was fake t=9s.
	if h.started.Load() != nativepeer.ProcessConcurrentAttempts+1 {
		t.Fatal("unexpected provider start count", h.started.Load())
	}
	h.clock.Advance(time.Second)
	receiveTest(t, h.receiver.done)
	receiveTest(t, h.sender.done)
}
func TestRemoteReceiverExpiryCancelsQueuedSenderBeforeProviderStartup(t *testing.T) {
	h := newQueuedHandshake(t)
	receiverPrep := receiveTest(t, h.receiverTimers.created)
	// The receiver starts before the offer reaches the sender. Firing only its
	// deadline reproduces remote expiry while sender capacity remains saturated.
	receiverPrep.timer.Fire()
	receiveTest(t, h.operation.cancelled)
	receiveTest(t, h.sender.done)
	_ = h.blockers[0].Close()
	if h.started.Load() != nativepeer.ProcessConcurrentAttempts {
		t.Fatal("expired remote operation started a provider", h.started.Load())
	}
	for _, blocker := range h.blockers[1:] {
		if blocker.ConnectionState() == pion.PeerConnectionStateClosed {
			t.Fatal("another session was cancelled")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), peerTestTimeout)
	defer cancel()
	next, err := h.native.NewPeerConnection(ctx, nativepeer.AttemptRequest{ProtocolSessionID: [16]byte{77}, Binding: testBinding(77)})
	if err != nil {
		t.Fatal("cancelled queue leaked another session's capacity", err)
	}
	_ = next.Close()
}

func TestSenderPreparationExpiresWhileProcessCapacityRemainsQueued(t *testing.T) {
	h := newQueuedHandshake(t)
	preparation := receiveTest(t, h.senderTimers.created)
	preparation.timer.Fire()
	receiveTest(t, h.sender.done)
	failure := receiveTest(t, h.session.failures)
	if failure.code != protocolsession.PeerOperationCodeTimeout {
		t.Fatalf("queue timeout lost attempt-scoped phase cause: %+v", failure)
	}
	if h.started.Load() != nativepeer.ProcessConcurrentAttempts {
		t.Fatal("expired preparation started a provider")
	}
}
