package sessionruntime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type admissionBarrierChannel struct {
	protocolsession.FrameChannel
	entered chan struct{}
	once    sync.Once
}

type terminalGateChannel struct {
	protocolsession.FrameChannel
	entered chan struct{}
	release chan struct{}
	sendErr error
	once    sync.Once
}

// receiveRetirementGateChannel separates transport retirement from the pump's
// asynchronous observation of Recv closure. Delaying cleanup publication makes
// the selection-to-send retirement ordering deterministic.
type receiveRetirementGateChannel struct {
	protocolsession.FrameChannel
	receive         chan framechannel.Frame
	retired         chan struct{}
	terminalEntered chan struct{}
	release         chan struct{}
	released        sync.Once
	terminalOnce    sync.Once
}

type failingTerminalSealer struct{ err error }

func (sealer failingTerminalSealer) NextSequence() (uint64, error) { return 1, nil }
func (sealer failingTerminalSealer) Seal([]byte) (protocolsession.SealedEnvelope, error) {
	return protocolsession.SealedEnvelope{}, sealer.err
}

func (channel *terminalGateChannel) SendTerminal(context.Context, framechannel.Frame) error {
	channel.once.Do(func() { close(channel.entered) })
	<-channel.release
	return channel.sendErr
}

func newReceiveRetirementGateChannel(
	channel protocolsession.FrameChannel,
) *receiveRetirementGateChannel {
	gate := &receiveRetirementGateChannel{
		FrameChannel:    channel,
		receive:         make(chan framechannel.Frame),
		retired:         make(chan struct{}),
		terminalEntered: make(chan struct{}),
		release:         make(chan struct{}),
	}
	go func() {
		for frame := range channel.Recv() {
			gate.receive <- frame
		}
		close(gate.retired)
		<-gate.release
		close(gate.receive)
	}()
	return gate
}

func (channel *receiveRetirementGateChannel) Recv() <-chan framechannel.Frame {
	return channel.receive
}

func (channel *receiveRetirementGateChannel) SendTerminal(
	ctx context.Context,
	frame framechannel.Frame,
) error {
	channel.terminalOnce.Do(func() { close(channel.terminalEntered) })
	err := channel.FrameChannel.SendTerminal(ctx, frame)
	if err == nil {
		return nil
	}
	return framechannel.RetireSend(err)
}

func (channel *receiveRetirementGateChannel) releaseRetirement() {
	channel.released.Do(func() { close(channel.release) })
}

func TestSenderFactoryInvalidTerminalDoesNotCommitStopBeforeValidRetry(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	for _, invalid := range []string{
		string([]byte{0xff}),
		"e\u0301",
		strings.Repeat("x", MaximumTerminalMessageBytes+1),
	} {
		if err := fixture.senderFactory.Stop(context.Background(), invalid); !errors.Is(err, ErrRuntimeConfig) {
			t.Fatalf("invalid terminal message error=%v", err)
		}
		fixture.senderFactory.mu.Lock()
		stopping := fixture.senderFactory.stopping
		fixture.senderFactory.mu.Unlock()
		if stopping {
			t.Fatal("invalid terminal message committed the factory stop")
		}
		select {
		case <-fixture.senderFactory.terminalDone:
			t.Fatal("invalid terminal message published terminal completion")
		default:
		}
	}
	if _, err := receiver.RequestLane(context.Background(), 0); err != nil {
		t.Fatalf("invalid factory stop damaged active session: %v", err)
	}
	if err := fixture.senderFactory.Stop(context.Background(), "valid stop"); err != nil {
		t.Fatalf("valid stop after rejected messages: %v", err)
	}
	select {
	case <-sender.Done():
	default:
		t.Fatal("valid factory stop did not finish the active sender session")
	}
}

func TestTerminalCallbacksCanReenterNonblockingStopInitiation(t *testing.T) {
	t.Run("factory connectivity callbacks", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		var callbackErrs = make(chan error, 2)
		fixture.senderFactory.terminalConnectivity = TerminalConnectivityFuncs{
			StopRecoveryFunc: func() {
				callbackErrs <- fixture.senderFactory.BeginStop("reentrant recovery stop")
			},
			CleanupFunc: func(context.Context) error {
				callbackErrs <- fixture.senderFactory.BeginStop("reentrant cleanup stop")
				return nil
			},
		}
		if err := fixture.senderFactory.BeginStop("initial stop"); err != nil {
			t.Fatal(err)
		}
		select {
		case <-fixture.senderFactory.terminalDone:
		case <-time.After(time.Second):
			t.Fatal("reentrant factory callbacks deadlocked terminal completion")
		}
		for range 2 {
			if err := <-callbackErrs; err != nil {
				t.Fatalf("reentrant factory BeginStop error=%v", err)
			}
		}
	})

	t.Run("transport send callback", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		senderChannel, receiverChannel := newMemoryChannelPair()
		accepted := make(chan struct {
			runtime *SenderRuntime
			err     error
		}, 1)
		go func() {
			runtime, err := fixture.senderFactory.Accept(context.Background(), senderChannel)
			accepted <- struct {
				runtime *SenderRuntime
				err     error
			}{runtime: runtime, err: err}
		}()
		receiver, err := fixture.receiverFactory.Connect(context.Background(), receiverChannel)
		if err != nil {
			t.Fatal(err)
		}
		acceptedResult := <-accepted
		if acceptedResult.err != nil {
			receiver.Close()
			t.Fatal(acceptedResult.err)
		}
		sender := acceptedResult.runtime
		defer sender.Close()
		defer receiver.Close()
		callbackErr := make(chan error, 1)
		var callbackOnce sync.Once
		senderChannel.pipe.mu.Lock()
		senderChannel.onSend = func(framechannel.Frame) {
			callbackOnce.Do(func() {
				callbackErr <- sender.BeginStop(context.Background(), "reentrant terminal send")
			})
		}
		senderChannel.pipe.mu.Unlock()
		if err := sender.Stop(context.Background(), "initial terminal send"); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-callbackErr:
			if err != nil {
				t.Fatalf("reentrant runtime BeginStop error=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("terminal send callback did not reenter BeginStop")
		}
	})
}

func (channel *admissionBarrierChannel) Recv() <-chan framechannel.Frame {
	channel.once.Do(func() { close(channel.entered) })
	return channel.FrameChannel.Recv()
}

func TestSenderFactoryStopWaitsForHandshakeAdmissionsWithoutCapturingCallerContext(t *testing.T) {
	tests := []struct {
		name  string
		start func(*SenderFactory, protocolsession.FrameChannel) <-chan error
	}{
		{
			name: "Accept",
			start: func(factory *SenderFactory, channel protocolsession.FrameChannel) <-chan error {
				result := make(chan error, 1)
				go func() {
					_, err := factory.Accept(context.Background(), channel)
					result <- err
				}()
				return result
			},
		},
		{
			name: "AdmitChannel",
			start: func(factory *SenderFactory, channel protocolsession.FrameChannel) <-chan error {
				result := make(chan error, 1)
				go func() {
					_, err := factory.AdmitChannel(context.Background(), channel)
					result <- err
				}()
				return result
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newVerticalFixture(t)
			base, peer := newMemoryChannelPair()
			defer base.Close()
			defer peer.Close()
			channel := &admissionBarrierChannel{FrameChannel: base, entered: make(chan struct{})}
			admissionResult := test.start(fixture.senderFactory, channel)
			<-channel.entered

			stopContext, cancelStop := context.WithCancel(context.Background())
			cancelStop()
			if err := fixture.senderFactory.Stop(stopContext, "test stop"); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("Stop with canceled caller context error=%v", err)
			}

			select {
			case err := <-admissionResult:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("terminal-canceled handshake error=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("handshake admission did not release its terminal barrier")
			}
			select {
			case <-fixture.senderFactory.terminalDone:
			case <-time.After(time.Second):
				t.Fatal("terminal cleanup did not resume after handshake admission drained")
			}
			if err := fixture.senderFactory.Stop(context.Background(), "test stop"); err != nil {
				t.Fatalf("second Stop after admission drain: %v", err)
			}
			fixture.senderFactory.mu.Lock()
			sessions := len(fixture.senderFactory.sessions)
			fixture.senderFactory.mu.Unlock()
			if sessions != 0 {
				t.Fatalf("terminal cleanup retained %d sessions", sessions)
			}
		})
	}
}

func TestSenderFactoryStopJoinsForcedRuntimeCleanupBeforeSecretsAndConnectivity(t *testing.T) {
	fixture := newVerticalFixture(t)
	fixture.senderFactory.terminalTimeout = 10 * time.Millisecond
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer receiver.Close()
	finalizerEntered := make(chan struct{})
	releaseFinalizer := make(chan struct{})
	if err := sender.addFinalizer(func() {
		close(finalizerEntered)
		<-releaseFinalizer
	}); err != nil {
		t.Fatal(err)
	}
	fixture.senderFactory.mu.Lock()
	ownedAuthKey := fixture.senderFactory.authKey
	ownedPrivateKey := fixture.senderFactory.privateKey
	fixture.senderFactory.mu.Unlock()
	if err := fixture.senderFactory.BeginStop("forced cleanup ordering"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finalizerEntered:
	case <-time.After(time.Second):
		t.Fatal("forced runtime close did not enter its finalizer")
	}
	select {
	case <-fixture.senderFactory.terminalDone:
		t.Fatal("factory completed while a runtime finalizer still owned shared resources")
	default:
	}
	if fixture.terminal.cleanups.Load() != 0 {
		t.Fatal("connectivity cleanup ran before the forced runtime join")
	}
	fixture.senderFactory.mu.Lock()
	retainedAuthKey := len(fixture.senderFactory.authKey) != 0
	retainedPrivateKey := len(fixture.senderFactory.privateKey) != 0
	fixture.senderFactory.mu.Unlock()
	if !retainedAuthKey || !retainedPrivateKey {
		t.Fatal("factory cleared secrets before the forced runtime join")
	}
	close(releaseFinalizer)
	select {
	case <-fixture.senderFactory.terminalDone:
	case <-time.After(time.Second):
		t.Fatal("factory did not complete after the runtime finalizer released")
	}
	if fixture.terminal.cleanups.Load() != 1 {
		t.Fatalf("connectivity cleanups=%d", fixture.terminal.cleanups.Load())
	}
	for name, secret := range map[string][]byte{"authentication": ownedAuthKey, "private": ownedPrivateKey} {
		for index, value := range secret {
			if value != 0 {
				t.Fatalf("%s key byte %d was not cleared", name, index)
			}
		}
	}
	fixture.senderFactory.mu.Lock()
	authKey, privateKey := fixture.senderFactory.authKey, fixture.senderFactory.privateKey
	fixture.senderFactory.mu.Unlock()
	if authKey != nil || privateKey != nil {
		t.Fatal("stopped factory retained secret slices")
	}
	fixture.senderFactory.mu.Lock()
	retainedDependencies := fixture.senderFactory.catalog != nil || fixture.senderFactory.content != nil ||
		fixture.senderFactory.peers != nil || fixture.senderFactory.replay != nil ||
		fixture.senderFactory.random != nil || fixture.senderFactory.laneIDs != nil ||
		fixture.senderFactory.now != nil || fixture.senderFactory.terminalConnectivity != nil ||
		fixture.senderFactory.terminalObserver != nil ||
		fixture.senderFactory.admissionContext != nil || fixture.senderFactory.cancelAdmissions != nil ||
		fixture.senderFactory.sessions != nil
	fixture.senderFactory.mu.Unlock()
	if retainedDependencies {
		t.Fatal("stopped sender factory retained its borrowed dependency graph")
	}
}

func TestSenderRuntimeCompositeTrackingOutlivesCoreUntilTerminalWorkerReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		coreDone := make(chan struct{})
		close(coreDone)
		sessionID := id16[protocolsession.ProtocolSessionID](77)
		runtime := &SenderRuntime{
			runtimeCore: &runtimeCore{ctx: ctx, done: coreDone},
			stopStarted: true, stopDone: make(chan struct{}), compositeDone: make(chan struct{}),
		}
		factory := &SenderFactory{sessions: map[protocolsession.ProtocolSessionID]*SenderRuntime{sessionID: runtime}}
		runtime.trackComposite(factory, sessionID)
		waited := make(chan struct{})
		go func() { runtime.WaitClosed(); close(waited) }()
		synctest.Wait()
		select {
		case <-waited:
			t.Fatal("WaitClosed returned while the terminal worker was still live")
		default:
		}
		factory.mu.Lock()
		tracked := factory.sessions[sessionID] == runtime
		factory.mu.Unlock()
		if !tracked {
			t.Fatal("factory dropped signing-key borrower at core completion")
		}
		closedRuntime := &SenderRuntime{runtimeCore: &runtimeCore{ctx: ctx, done: coreDone}}
		if err := closedRuntime.BeginStop(context.Background(), "late stop"); !errors.Is(err, ErrRuntimeClosed) {
			t.Fatalf("BeginStop after core closure error=%v", err)
		}
		close(runtime.stopDone)
		synctest.Wait()
		select {
		case <-waited:
		default:
			t.Fatal("WaitClosed did not join terminal-worker completion")
		}
		factory.mu.Lock()
		_, tracked = factory.sessions[sessionID]
		factory.mu.Unlock()
		if tracked {
			t.Fatal("factory retained runtime after composite completion")
		}
	})
}

func TestSenderFactoryStopWaitsForContextIgnoringTerminalTransport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newVerticalFixture(t)
		baseSenderChannel, receiverChannel := newMemoryChannelPair()
		gate := &terminalGateChannel{
			FrameChannel: baseSenderChannel, entered: make(chan struct{}), release: make(chan struct{}),
		}
		accepted := make(chan struct {
			runtime *SenderRuntime
			err     error
		}, 1)
		go func() {
			runtime, err := fixture.senderFactory.Accept(context.Background(), gate)
			accepted <- struct {
				runtime *SenderRuntime
				err     error
			}{runtime: runtime, err: err}
		}()
		receiver, err := fixture.receiverFactory.Connect(context.Background(), receiverChannel)
		if err != nil {
			t.Fatal(err)
		}
		defer receiver.Close()
		acceptedResult := <-accepted
		if acceptedResult.err != nil {
			t.Fatal(acceptedResult.err)
		}
		sender := acceptedResult.runtime
		fixture.senderFactory.mu.Lock()
		ownedPrivateKey := fixture.senderFactory.privateKey
		fixture.senderFactory.mu.Unlock()
		if err := sender.BeginStop(context.Background(), "gated terminal"); err != nil {
			t.Fatal(err)
		}
		<-gate.entered
		sender.BeginClose()
		if err := fixture.senderFactory.BeginStop("factory stop"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case <-fixture.senderFactory.terminalDone:
			t.Fatal("factory completed while terminal transport still borrowed runtime ownership")
		default:
		}
		fixture.senderFactory.mu.Lock()
		retainedPrivateKey := len(fixture.senderFactory.privateKey) != 0
		fixture.senderFactory.mu.Unlock()
		if !retainedPrivateKey {
			t.Fatal("factory cleared its signing key while terminal transport was live")
		}
		close(gate.release)
		synctest.Wait()
		select {
		case <-fixture.senderFactory.terminalDone:
		default:
			t.Fatal("factory did not complete after terminal transport returned")
		}
		for index, value := range ownedPrivateKey {
			if value != 0 {
				t.Fatalf("factory private key byte %d was not cleared", index)
			}
		}
	})
}

func TestSenderFactoryStopTreatsTransportRetirementBeforePhysicalAdmissionAsNatural(t *testing.T) {
	fixture := newVerticalFixture(t)
	observed := make(chan SenderTerminalObservation, 1)
	fixture.senderFactory.terminalObserver = SenderTerminalObserverFunc(
		func(observation SenderTerminalObservation) { observed <- observation },
	)
	baseSenderChannel, receiverChannel := newMemoryChannelPair()
	senderChannel := newReceiveRetirementGateChannel(baseSenderChannel)
	t.Cleanup(senderChannel.releaseRetirement)
	accepted := make(chan struct {
		runtime *SenderRuntime
		err     error
	}, 1)
	go func() {
		runtime, err := fixture.senderFactory.Accept(context.Background(), senderChannel)
		accepted <- struct {
			runtime *SenderRuntime
			err     error
		}{runtime: runtime, err: err}
	}()
	receiver, err := fixture.receiverFactory.Connect(context.Background(), receiverChannel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Close)
	acceptedResult := <-accepted
	if acceptedResult.err != nil {
		t.Fatal(acceptedResult.err)
	}
	sender := acceptedResult.runtime
	t.Cleanup(sender.Close)

	if err := baseSenderChannel.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-senderChannel.retired:
	case <-time.After(time.Second):
		t.Fatal("transport retirement did not reach the receive-publication barrier")
	}
	sender.lanes.mu.Lock()
	lane := sender.lanes.active[sender.initial.ID]
	tracked := lane != nil && lane.identity == sender.initial
	closing := lane == nil || lane.closing
	writerAccepting := lane != nil && lane.writer.Accepting()
	sender.lanes.mu.Unlock()
	if !tracked || closing || !writerAccepting || sender.ctx.Err() != nil {
		t.Fatalf(
			"pre-detach retirement state: tracked=%t closing=%t writer_accepting=%t context=%v",
			tracked, closing, writerAccepting, sender.ctx.Err(),
		)
	}
	if state := senderChannel.State(); state != framechannel.Closed {
		t.Fatalf("transport state=%d, want closed", state)
	}
	if recipients := sender.lanes.snapshot(); len(recipients) != 1 {
		t.Fatalf("pre-detach transport recipients=%d, want stale snapshot window", len(recipients))
	}
	if _, err := sender.lanes.selectLane(nil); err != nil {
		t.Fatalf("pre-detach lane selection did not expose retirement race: %v", err)
	}

	if err := fixture.senderFactory.Stop(context.Background(), "share cancelled"); err != nil {
		t.Fatalf("transport retirement before terminal adoption failed factory stop: %v", err)
	}
	select {
	case <-senderChannel.terminalEntered:
	default:
		t.Fatal("terminal writer did not reach the retired transport")
	}
	select {
	case observation := <-observed:
		if observation.ProtocolSessionID != sender.sessionID || observation.Lane != sender.initial ||
			observation.TransportDisposition != SenderTerminalTransportRetired ||
			observation.Outcome != SenderTerminalOutcomeDropped ||
			observation.Decision != SenderTerminalDecisionNaturalRetirement {
			t.Fatalf("retirement observation=%+v", observation)
		}
	default:
		t.Fatal("retired terminal admission did not emit its lifecycle decision")
	}
}

func TestSenderFactoryStopTreatsLastLaneDetachBeforeTerminalAdmissionAsComplete(t *testing.T) {
	fixture := newVerticalFixture(t)
	senderChannel, receiverChannel := newMemoryChannelPair()
	accepted := make(chan struct {
		runtime *SenderRuntime
		err     error
	}, 1)
	go func() {
		runtime, err := fixture.senderFactory.Accept(context.Background(), senderChannel)
		accepted <- struct {
			runtime *SenderRuntime
			err     error
		}{runtime: runtime, err: err}
	}()
	receiver, err := fixture.receiverFactory.Connect(context.Background(), receiverChannel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Close)
	acceptedResult := <-accepted
	if acceptedResult.err != nil {
		t.Fatal(acceptedResult.err)
	}
	sender := acceptedResult.runtime

	sender.lanes.mu.Lock()
	releaseRegistry := sender.lanes.onDetach
	sender.lanes.mu.Unlock()
	detachEntered := make(chan struct{})
	detachRelease := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(detachRelease)
		}
	}()
	sender.lanes.setDetachHook(func(identity LaneIdentity) {
		close(detachEntered)
		<-detachRelease
		if releaseRegistry != nil {
			releaseRegistry(identity)
		}
	})
	if err := senderChannel.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-detachEntered:
	case <-time.After(time.Second):
		t.Fatal("last-lane detach did not reach the publication barrier")
	}
	if sender.ctx.Err() != nil || len(sender.lanes.snapshot()) != 0 {
		t.Fatalf("detach barrier state: context=%v recipients=%d", sender.ctx.Err(), len(sender.lanes.snapshot()))
	}

	if err := sender.BeginStop(context.Background(), "share cancelled"); err != nil {
		t.Fatalf("terminal initiation in detach publication window: %v", err)
	}
	stopResult := make(chan error, 1)
	go func() { stopResult <- fixture.senderFactory.Stop(context.Background(), "share cancelled") }()
	close(detachRelease)
	released = true
	sessionWaitContext, cancelSessionWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelSessionWait()
	if err := sender.WaitStopped(sessionWaitContext); err != nil {
		t.Fatalf("empty terminal fanout failed sender stop: %v", err)
	}
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatalf("empty terminal fanout failed factory stop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("factory stop did not join the detached runtime")
	}
}

func TestSenderFactoryStopPropagatesAcceptedTerminalDeliveryFailure(t *testing.T) {
	fixture := newVerticalFixture(t)
	observed := make(chan SenderTerminalObservation, 1)
	fixture.senderFactory.terminalObserver = SenderTerminalObserverFunc(
		func(observation SenderTerminalObservation) { observed <- observation },
	)
	baseSenderChannel, receiverChannel := newMemoryChannelPair()
	transportErr := errors.New("accepted terminal transport failed")
	gate := &terminalGateChannel{
		FrameChannel: baseSenderChannel,
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
		sendErr:      transportErr,
	}
	accepted := make(chan struct {
		runtime *SenderRuntime
		err     error
	}, 1)
	go func() {
		runtime, err := fixture.senderFactory.Accept(context.Background(), gate)
		accepted <- struct {
			runtime *SenderRuntime
			err     error
		}{runtime: runtime, err: err}
	}()
	receiver, err := fixture.receiverFactory.Connect(context.Background(), receiverChannel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Close)
	acceptedResult := <-accepted
	if acceptedResult.err != nil {
		t.Fatal(acceptedResult.err)
	}

	stopResult := make(chan error, 1)
	go func() { stopResult <- fixture.senderFactory.Stop(context.Background(), "share cancelled") }()
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("terminal receipt was not admitted")
	}
	// Admission owns the terminal outcome even if the physical lane closes
	// before the transport callback settles.
	_ = baseSenderChannel.Close()
	close(gate.release)
	select {
	case err := <-stopResult:
		if !errors.Is(err, transportErr) {
			t.Fatalf("accepted terminal failure = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("factory stop did not return the accepted terminal failure")
	}
	select {
	case observation := <-observed:
		if observation.TransportDisposition != SenderTerminalTransportAccepted ||
			observation.Outcome != SenderTerminalOutcomeUnknown ||
			observation.Decision != SenderTerminalDecisionFailed {
			t.Fatalf("accepted failure observation=%+v", observation)
		}
	default:
		t.Fatal("accepted terminal failure did not emit its lifecycle decision")
	}
}

func TestStopFactorySessionIgnoresErrorFromAlreadyEndedRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	close(done)
	runtime := &SenderRuntime{runtimeCore: &runtimeCore{ctx: ctx, done: done}}
	runtime.recordError(ErrLaneUnavailable)
	if err := stopFactorySession(context.Background(), runtime, "share cancelled"); err != nil {
		t.Fatalf("already-ended runtime polluted factory stop: %v", err)
	}
}

func TestEmptyTerminalFanoutDistinguishesCallerFromLifecycleCancellation(t *testing.T) {
	runtime := &runtimeCore{}
	runtime.lanes = newRuntimeLanes(runtime)
	outbound := senderOutbound{runtime: runtime}

	lifecycleContext, cancelLifecycle := context.WithCancel(context.Background())
	cancelLifecycle()
	if err := outbound.sendTerminalAll(lifecycleContext, context.Background(), nil); err != nil {
		t.Fatalf("natural lifecycle cancellation failed empty fanout: %v", err)
	}
	callerContext, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	if err := outbound.sendTerminalAll(context.Background(), callerContext, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation across empty fanout = %v", err)
	}
}

func TestTerminalPreAdmissionWriterStopRequiresNoUsableReplacement(t *testing.T) {
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: SessionStoppedCode, Message: "share cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	for _, test := range []struct {
		name           string
		addReplacement bool
		wantError      bool
	}{
		{name: "writer completion precedes lane closing"},
		{name: "usable replacement remains", addReplacement: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
			recipients := runtime.lanes.snapshot()
			if len(recipients) != 1 {
				t.Fatalf("terminal recipients = %d, want 1", len(recipients))
			}
			if test.addReplacement {
				replacement := newMemoryChannel(t)
				if _, err := runtime.lanes.add(
					LaneIdentity{ID: 2, Epoch: 1}, replacement, permissiveInboundAuthenticator(), false,
				); err != nil {
					t.Fatalf("add usable replacement: %v", err)
				}
			}
			writerContext, cancelWriter := context.WithCancel(context.Background())
			cancelWriter()
			if err := recipients[0].writer.Run(writerContext); !errors.Is(err, context.Canceled) {
				t.Fatalf("stop recipient writer: %v", err)
			}
			runtime.lanes.mu.Lock()
			initial := runtime.lanes.active[runtime.initial.ID]
			closing := initial == nil || initial.closing
			runtime.lanes.mu.Unlock()
			if closing {
				t.Fatal("test did not preserve the writer-Done-before-lane-closing window")
			}
			usable := runtime.lanes.hasUsable()
			current := runtime.lanes.snapshot()
			selected, selectErr := runtime.lanes.selectLane(nil)
			if test.addReplacement {
				if !usable || len(current) != 1 || current[0].identity.ID != 2 ||
					selectErr != nil || selected.identity.ID != 2 {
					t.Fatalf("replacement authority: usable=%v snapshot=%+v selected=%+v err=%v",
						usable, current, selected, selectErr)
				}
			} else if usable || len(current) != 0 || !errors.Is(selectErr, ErrLaneUnavailable) {
				t.Fatalf("stopped-writer authority: usable=%v snapshot=%+v selected=%+v err=%v",
					usable, current, selected, selectErr)
			}
			err := (senderOutbound{runtime: runtime, privateKey: privateKey}).sendTerminalRecipients(
				context.Background(), context.Background(), body, recipients,
			)
			if test.wantError {
				if !errors.Is(err, protocolsession.ErrWriterStopped) {
					t.Fatalf("usable stopped recipient error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("naturally retired recipients failed terminal fanout: %v", err)
			}
		})
	}
}

func TestTerminalPostAdmissionWriterStopRequiresNoUsableReplacement(t *testing.T) {
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: SessionStoppedCode, Message: "share cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	for _, test := range []struct {
		name           string
		addReplacement bool
		wantError      bool
		wantDecision   SenderTerminalDecision
	}{
		{name: "last writer retires", wantDecision: SenderTerminalDecisionNaturalRetirement},
		{
			name: "usable replacement remains", addReplacement: true, wantError: true,
			wantDecision: SenderTerminalDecisionFailed,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
				recipients := runtime.lanes.snapshot()
				if len(recipients) != 1 {
					t.Fatalf("terminal recipients = %d, want 1", len(recipients))
				}
				if test.addReplacement {
					replacement := newMemoryChannel(t)
					if _, err := runtime.lanes.add(
						LaneIdentity{ID: 2, Epoch: 1}, replacement, permissiveInboundAuthenticator(), false,
					); err != nil {
						t.Fatalf("add usable replacement: %v", err)
					}
				}
				observed := make(chan SenderTerminalObservation, 1)
				result := make(chan error, 1)
				go func() {
					result <- (senderOutbound{
						runtime: runtime, privateKey: privateKey,
						observer: SenderTerminalObserverFunc(
							func(observation SenderTerminalObservation) { observed <- observation },
						),
					}).sendTerminalRecipients(
						context.Background(), context.Background(), body, recipients,
					)
				}()
				// The sender is durably blocked on the admitted receipt before the
				// writer publishes its local retirement.
				synctest.Wait()
				writerContext, cancelWriter := context.WithCancel(context.Background())
				cancelWriter()
				if err := recipients[0].writer.Run(writerContext); !errors.Is(err, context.Canceled) {
					t.Fatalf("stop recipient writer: %v", err)
				}
				synctest.Wait()
				terminalErr := <-result
				if test.wantError {
					if !errors.Is(terminalErr, protocolsession.ErrWriterStopped) {
						t.Fatalf("usable stopped recipient error = %v", terminalErr)
					}
				} else if terminalErr != nil {
					t.Fatalf("naturally retired receipt failed terminal fanout: %v", terminalErr)
				}
				observation := <-observed
				if observation.TransportDisposition != SenderTerminalTransportNotReached ||
					observation.Outcome != SenderTerminalOutcomeDropped ||
					observation.Decision != test.wantDecision {
					t.Fatalf("post-admission retirement observation=%+v", observation)
				}
			})
		})
	}
}

func TestDeliveredTerminalPreservesCallerAndHardAdmissionFailures(t *testing.T) {
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: SessionStoppedCode, Message: "share cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("caller cancellation", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
		t.Cleanup(sender.Close)
		t.Cleanup(receiver.Close)
		callerContext, cancelCaller := context.WithCancel(context.Background())
		cancelCaller()
		err := sender.outbound.sendTerminalRecipients(
			context.Background(), callerContext, body, sender.lanes.snapshot(),
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("delivered terminal erased caller cancellation: %v", err)
		}
	})
	t.Run("hard preparation failure", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
		t.Cleanup(sender.Close)
		t.Cleanup(receiver.Close)
		recipients := append([]selectedLane{{identity: LaneIdentity{}}}, sender.lanes.snapshot()...)
		err := sender.outbound.sendTerminalRecipients(
			context.Background(), context.Background(), body, recipients,
		)
		if !errors.Is(err, protocolsession.ErrControlBinding) {
			t.Fatalf("delivered terminal erased hard preparation failure: %v", err)
		}
	})
	t.Run("accepted seal failure", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
		t.Cleanup(sender.Close)
		t.Cleanup(receiver.Close)
		policyRuntime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
		failedChannel := newMemoryChannel(t)
		sealErr := errors.New("terminal seal failed")
		failedWriter, err := protocolsession.NewSessionWriter(
			failedChannel,
			failingTerminalSealer{err: sealErr},
			runtimeLanePolicy{runtime: policyRuntime},
		)
		if err != nil {
			t.Fatal(err)
		}
		writerResult := make(chan error, 1)
		go func() { writerResult <- failedWriter.Run(context.Background()) }()
		recipients := append(
			[]selectedLane{{
				identity: LaneIdentity{ID: 99, Epoch: 1},
				channel:  failedChannel,
				writer:   failedWriter,
				done:     failedWriter.Done(),
			}},
			sender.lanes.snapshot()...,
		)
		err = sender.outbound.sendTerminalRecipients(
			context.Background(), context.Background(), body, recipients,
		)
		if !errors.Is(err, sealErr) {
			t.Fatalf("delivered terminal erased accepted seal failure: %v", err)
		}
		select {
		case err := <-writerResult:
			if !errors.Is(err, sealErr) {
				t.Fatalf("failing terminal writer result = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("failing terminal writer did not settle")
		}
	})
}
