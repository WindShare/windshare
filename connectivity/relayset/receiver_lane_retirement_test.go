package relayset

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	receiversession "github.com/windshare/windshare/core/session/receiversession"
	sendersession "github.com/windshare/windshare/core/session/sendersession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type memoryRelayFileSource struct {
	root catalog.NodeRecord
}

func (s *memoryRelayFileSource) OpenFileSource(_ context.Context, sc liveshare.FileSourceContext) (liveshare.FileSource, error) {
	id, err := sc.NewIdentity()
	if err != nil {
		return nil, err
	}
	dir, err := catalog.DirectoryIDFromBytes(id[:])
	if err != nil {
		return nil, err
	}
	sourceID, err := catalog.NewSourceIdentity([]byte("memory:root"))
	if err != nil {
		return nil, err
	}
	sourceRef, err := catalog.NewSourceReference([]byte("root"))
	if err != nil {
		return nil, err
	}
	s.root, err = catalog.NewDirectoryNodeRecord(dir, sc.SyntheticRoot, "root", sourceRef, sourceID, catalog.ModifiedTime{})
	return s, err
}
func (s *memoryRelayFileSource) SelectedRoots() []catalog.NodeRecord {
	return []catalog.NodeRecord{s.root}
}
func (*memoryRelayFileSource) ScanDirectory(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
	return catalog.ScanResult{}, errors.New("unused scan")
}
func (*memoryRelayFileSource) OpenStable(context.Context, catalog.NodeRecord) (content.StableFile, error) {
	return nil, errors.New("unused file")
}
func (*memoryRelayFileSource) Close() error { return nil }

type memoryRelayTerminal struct{}

func (memoryRelayTerminal) StopRecovery()                 {}
func (memoryRelayTerminal) Stop(context.Context) error    { return nil }
func (memoryRelayTerminal) Cleanup(context.Context) error { return nil }

type memoryRelayPeer struct{}

func (memoryRelayPeer) HandleMessage(context.Context, protocolsession.Operation, protocolsession.Message) error {
	return nil
}
func (memoryRelayPeer) Cancel(context.Context, protocolsession.Operation) error { return nil }
func (memoryRelayPeer) Run(ctx context.Context) error                           { <-ctx.Done(); return ctx.Err() }

type memoryPipe struct {
	mu     sync.Mutex
	closed [2]bool
	inbox  [2]chan framechannel.Frame
}

type pipeSendGate struct {
	once    sync.Once
	entered chan struct{}
	release <-chan struct{}
}

type memoryPipeEnd struct {
	pipe     *memoryPipe
	index    int
	sendGate *pipeSendGate
}

func (e *memoryPipeEnd) armSendGate(entered chan struct{}, release <-chan struct{}) {
	e.pipe.mu.Lock()
	e.sendGate = &pipeSendGate{entered: entered, release: release}
	e.pipe.mu.Unlock()
}

func newMemoryPipe() (*memoryPipeEnd, *memoryPipeEnd) {
	p := &memoryPipe{
		inbox: [2]chan framechannel.Frame{
			make(chan framechannel.Frame, 32),
			make(chan framechannel.Frame, 32),
		},
	}
	return &memoryPipeEnd{pipe: p, index: 0}, &memoryPipeEnd{pipe: p, index: 1}
}

func (e *memoryPipeEnd) Send(ctx context.Context, frame framechannel.Frame) error {
	peer := 1 - e.index
	e.pipe.mu.Lock()
	gate := e.sendGate
	e.pipe.mu.Unlock()
	if gate != nil {
		gate.once.Do(func() { close(gate.entered) })
		<-gate.release
	}
	e.pipe.mu.Lock()
	if e.pipe.closed[e.index] || e.pipe.closed[peer] {
		e.pipe.mu.Unlock()
		return framechannel.RetireSend(io.ErrClosedPipe)
	}
	copied := append(framechannel.Frame(nil), frame...)
	select {
	case e.pipe.inbox[peer] <- copied:
		e.pipe.mu.Unlock()
		return nil
	default:
		e.pipe.mu.Unlock()
		return ctx.Err()
	}
}

func (e *memoryPipeEnd) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	if err := e.Send(ctx, frame); err != nil {
		return err
	}
	return e.Close()
}

func (e *memoryPipeEnd) Recv() <-chan framechannel.Frame { return e.pipe.inbox[e.index] }

func (e *memoryPipeEnd) State() framechannel.ChannelState {
	e.pipe.mu.Lock()
	defer e.pipe.mu.Unlock()
	if e.pipe.closed[e.index] {
		return framechannel.Closed
	}
	return framechannel.Open
}

func (e *memoryPipeEnd) Close() error {
	e.pipe.mu.Lock()
	defer e.pipe.mu.Unlock()
	for i := range 2 {
		if !e.pipe.closed[i] {
			e.pipe.closed[i] = true
			close(e.pipe.inbox[i])
		}
	}
	return nil
}

type stagedCloseReceiverConnection struct {
	descriptor []byte
	channel    *memoryPipeEnd
	endpoint   v2.RelayEndpoint
	done       chan struct{}
	doneOnce   sync.Once
	mu         sync.Mutex
	err        error
}

func (c *stagedCloseReceiverConnection) Descriptor() []byte {
	return append([]byte(nil), c.descriptor...)
}
func (c *stagedCloseReceiverConnection) Channel() protocolsession.FrameChannel { return c.channel }
func (c *stagedCloseReceiverConnection) Endpoint() v2.RelayEndpoint            { return c.endpoint }
func (c *stagedCloseReceiverConnection) Done() <-chan struct{}                 { return c.done }
func (c *stagedCloseReceiverConnection) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
func (c *stagedCloseReceiverConnection) signalDone(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.doneOnce.Do(func() { close(c.done) })
}
func (c *stagedCloseReceiverConnection) Close() error {
	c.signalDone(nil)
	return c.channel.Close()
}

func newInMemoryLiveSender(t *testing.T, relayURLs ...string) (*liveshare.PreparedSender, *sendersession.Factory) {
	t.Helper()
	pool, err := content.NewHandlePool(content.DefaultHandlePoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	catalogBudget, err := catalog.NewBudgetAccount("relayset-unit", catalog.DefaultProcessBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	cacheBudget, err := contentflow.NewProcessCacheBudget(uint64(1) << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cacheBudget.Close() })
	sender, err := liveshare.PrepareSender(context.Background(), liveshare.SenderConfig{
		Source:           &memoryRelayFileSource{},
		Relays:           relayURLs,
		ChunkSize:        catalog.MinChunkSize,
		RevisionCapacity: pool,
		CatalogBudget:    catalogBudget,
		CacheBudget:      cacheBudget,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	if err := sender.AuthorizeRegistration(); err != nil {
		t.Fatal(err)
	}
	factory, err := sender.NewRuntimeFactory(liveshare.RuntimeFactoryConfig{
		TerminalConnectivity: memoryRelayTerminal{},
		PeerHandlers: sendersession.PeerHandlerFactoryFunc(func(sendersession.PeerSession) (sendersession.PeerHandler, error) {
			return memoryRelayPeer{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return sender, factory
}

func TestReceiverSingleLaneLossDoesNotRetryDyingSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const relayURL = "http://127.0.0.1:18080"
		sender, factory := newInMemoryLiveSender(t, relayURL)
		endpoint, err := v2.NormalizeRelayEndpoint(relayURL)
		if err != nil {
			t.Fatal(err)
		}
		clock := &receiverFakeClock{now: time.Unix(1, 0)}
		var dials atomic.Int32
		var mu sync.Mutex
		var observations []ReceiverRecoveryObservation
		var firstConn *stagedCloseReceiverConnection
		var senderDone sync.WaitGroup
		recovery, err := NewReceiverRecovery(ReceiverRecoveryOptions{
			Clock:  clock,
			Jitter: func(delay time.Duration) time.Duration { return delay },
			Observe: func(obs ReceiverRecoveryObservation) {
				mu.Lock()
				observations = append(observations, obs)
				mu.Unlock()
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		set, err := recovery.Join(context.Background(), ReceiverConfig{
			Receiver: liveshare.ReceiverConfig{
				Capacity:   testReceiveCapacity(t),
				Capability: sender.Capability(),
			},
			Dial: func(ctx context.Context, _ relayv2.ReceiverConfig) (ReceiverConnection, error) {
				if dials.Add(1) > 1 {
					return nil, errors.New("unexpected retry dial on dying session")
				}
				senderEnd, receiverEnd := newMemoryPipe()
				senderDone.Go(func() {
					admission, admitErr := factory.AdmitChannel(context.Background(), senderEnd)
					if admitErr != nil {
						_ = senderEnd.Close()
						return
					}
					if admission.Session != nil {
						<-admission.Session.Done()
						admission.Session.Close()
					}
				})
				firstConn = &stagedCloseReceiverConnection{
					descriptor: sender.Registration().Descriptor,
					channel:    receiverEnd,
					endpoint:   endpoint,
					done:       make(chan struct{}),
				}
				return firstConn, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			set.Close()
			senderDone.Wait()
		}()
		runtime, _, err := set.WaitReady(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		// Simulate transport connection.Done closing before sessioncore finishes
		// retiring the sole lane. Receiver must wait for lane retirement instead
		// of scheduling an in-session retry against the dying generation.
		firstConn.signalDone(errors.New("websocket EOF"))
		synctest.Wait()
		if dials.Load() != 1 {
			t.Fatalf("dials=%d after connection.Done before lane retirement, want 1", dials.Load())
		}
		clock.mu.Lock()
		earlyWaits := append([]time.Duration(nil), clock.waits...)
		clock.mu.Unlock()
		if len(earlyWaits) != 0 {
			t.Fatalf("virtual clock advanced before lane retirement: waits=%v", earlyWaits)
		}

		if err := firstConn.channel.Close(); err != nil {
			t.Fatal(err)
		}
		<-runtime.Done()
		set.Close()
		senderDone.Wait()

		clock.mu.Lock()
		finalWaits := append([]time.Duration(nil), clock.waits...)
		clock.mu.Unlock()
		if dials.Load() != 1 || len(finalWaits) != 0 {
			t.Fatalf("dying single-lane session retried: dials=%d waits=%v", dials.Load(), finalWaits)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, obs := range observations {
			if obs.Phase == ReceiverRecoveryRetrying || obs.Phase == ReceiverRecoveryWaiting {
				t.Fatalf("dying single-lane session emitted retry observation: %+v", obs)
			}
		}
	})
}

func TestReceiverWaitForDemandStopsWhenRuntimeStoppingWithZeroLanes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const relayURL = "http://127.0.0.1:18081"
		sender, factory := newInMemoryLiveSender(t, relayURL)
		endpoint, err := v2.NormalizeRelayEndpoint(relayURL)
		if err != nil {
			t.Fatal(err)
		}
		var firstConn *stagedCloseReceiverConnection
		var senderDone sync.WaitGroup
		clock := &receiverFakeClock{now: time.Unix(1, 0)}
		set, err := NewReceiver(context.Background(), ReceiverConfig{
			Clock: clock,
			Receiver: liveshare.ReceiverConfig{
				Capacity:   testReceiveCapacity(t),
				Capability: sender.Capability(),
			},
			Dial: func(context.Context, relayv2.ReceiverConfig) (ReceiverConnection, error) {
				senderEnd, receiverEnd := newMemoryPipe()
				senderDone.Go(func() {
					admission, admitErr := factory.AdmitChannel(context.Background(), senderEnd)
					if admitErr != nil {
						_ = senderEnd.Close()
						return
					}
					if admission.Session != nil {
						<-admission.Session.Done()
						admission.Session.Close()
					}
				})
				firstConn = &stagedCloseReceiverConnection{
					descriptor: sender.Registration().Descriptor,
					channel:    receiverEnd,
					endpoint:   endpoint,
					done:       make(chan struct{}),
				}
				return firstConn, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			set.Close()
			senderDone.Wait()
		}()
		runtime, _, err := set.WaitReady(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := firstConn.channel.Close(); err != nil {
			t.Fatal(err)
		}
		<-runtime.Done()
		if !runtime.Stopping() || runtime.AttachedLanes() != 0 {
			t.Fatalf("expected stopped runtime with zero lanes: stopping=%v lanes=%d", runtime.Stopping(), runtime.AttachedLanes())
		}
		if err := set.waitForDemand(context.Background()); !errors.Is(err, session.ErrRuntimeClosed) {
			t.Fatalf("waitForDemand on stopped zero-lane runtime = %v, want %v", err, session.ErrRuntimeClosed)
		}
	})
}

func TestReceiverPublishInitialRejectsRetiredCandidateAndPreservesCompetingRelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const relayURL = "http://127.0.0.1:18082"
		sender, factory := newInMemoryLiveSender(t, relayURL)
		endpoint, err := v2.NormalizeRelayEndpoint(relayURL)
		if err != nil {
			t.Fatal(err)
		}
		recovery, err := NewReceiverRecovery(ReceiverRecoveryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		lifetime, cancel := context.WithCancel(context.Background())
		defer cancel()
		receiver := &Receiver{
			ctx:       lifetime,
			cancel:    cancel,
			config:    ReceiverConfig{recovery: recovery},
			remaining: 2,
			ready:     make(chan struct{}),
		}
		var senderDone sync.WaitGroup
		defer func() {
			receiver.Close()
			senderDone.Wait()
		}()
		connectCandidate := func() (*receiversession.Runtime, *liveshare.PreparedReceiver, *stagedCloseReceiverConnection) {
			senderEnd, receiverEnd := newMemoryPipe()
			senderDone.Go(func() {
				admission, admitErr := factory.AdmitChannel(context.Background(), senderEnd)
				if admitErr != nil {
					_ = senderEnd.Close()
					return
				}
				if admission.Session != nil {
					<-admission.Session.Done()
					admission.Session.Close()
				}
			})
			prepared, prepErr := liveshare.PrepareReceiver(liveshare.ReceiverConfig{
				Capacity:         testReceiveCapacity(t),
				Capability:       sender.Capability(),
				DescriptorObject: sender.Registration().Descriptor,
			})
			if prepErr != nil {
				t.Fatal(prepErr)
			}
			conn := &stagedCloseReceiverConnection{
				descriptor: sender.Registration().Descriptor,
				channel:    receiverEnd,
				endpoint:   endpoint,
				done:       make(chan struct{}),
			}
			candidate, connErr := prepared.Connect(context.Background(), receiverEnd, transferlanes.RouteRelay)
			if connErr != nil {
				prepared.Close()
				t.Fatal(connErr)
			}
			return candidate, prepared, conn
		}

		deadCandidate, deadPrepared, deadConn := connectCandidate()
		defer func() {
			deadCandidate.Close()
			deadPrepared.Close()
			_ = deadConn.Close()
		}()
		if err := deadConn.channel.Close(); err != nil {
			t.Fatal(err)
		}
		<-deadCandidate.Done()

		var deadLane admittedLane
		if err := receiver.publishInitial(context.Background(), deadCandidate, deadPrepared, deadConn, &deadLane); !errors.Is(err, session.ErrRuntimeClosed) {
			t.Fatalf("publishInitial on retired candidate = %v, want %v", err, session.ErrRuntimeClosed)
		}
		if receiver.current() != nil || receiver.stopRuntimeWatch != nil || receiver.ctx.Err() != nil {
			t.Fatalf("retired candidate mutated receiver: runtime=%v watch=%v ctxErr=%v", receiver.current(), receiver.stopRuntimeWatch != nil, receiver.ctx.Err())
		}
		if deadLane.done != nil || deadLane.identity != (session.LaneIdentity{}) {
			t.Fatalf("retired candidate mutated lane state: %+v", deadLane)
		}
		select {
		case <-receiver.ready:
			t.Fatal("retired candidate closed ready channel")
		default:
		}

		liveCandidate, livePrepared, liveConn := connectCandidate()
		defer func() { _ = liveConn.Close() }()
		var liveLane admittedLane
		if err := receiver.publishInitial(context.Background(), liveCandidate, livePrepared, liveConn, &liveLane); err != nil {
			liveCandidate.Close()
			livePrepared.Close()
			t.Fatalf("competing live candidate failed to publish: %v", err)
		}
		if liveLane.done == nil || liveLane.done == closedReceiverLane {
			t.Fatalf("published live candidate did not bind live lane done channel: %+v", liveLane)
		}
		gotRuntime, gotConn, err := receiver.WaitReady(context.Background())
		if err != nil || gotRuntime != liveCandidate || gotConn != liveConn {
			t.Fatalf("WaitReady = (%v, %v, %v), want live candidate", gotRuntime, gotConn, err)
		}
	})
}

func TestReceiverDetachAndClosingSoleLaneWindowsDoNotRetryDyingSession(t *testing.T) {
	controlBody, err := protocolsession.EncodePeerPathControl(protocolsession.PeerPathControl{
		PeerPathID:          [16]byte{1},
		NetworkGenerationID: [16]byte{2},
		ControlSequence:     1,
		Kind:                protocolsession.PeerPathRevoke,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name              string
		detachExplicitly  bool
		wantAttachedLanes int
	}{
		{name: "explicit detach zero-lane window", detachExplicitly: true, wantAttachedLanes: 0},
		{name: "natural markClosing drain window", detachExplicitly: false, wantAttachedLanes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const relayA = "http://127.0.0.1:18084"
				const relayB = "http://127.0.0.1:18085"
				sender, factory := newInMemoryLiveSender(t, relayA, relayB)
				endpointA, err := v2.NormalizeRelayEndpoint(relayA)
				if err != nil {
					t.Fatal(err)
				}
				var relayADials, relayBDials atomic.Int32
				var obsMu sync.Mutex
				var observations []ReceiverRecoveryObservation
				var pipeMu sync.Mutex
				var senderPipe, receiverPipe *memoryPipeEnd
				var senderDone sync.WaitGroup
				recovery, err := NewReceiverRecovery(ReceiverRecoveryOptions{
					Jitter: func(delay time.Duration) time.Duration { return delay },
					Observe: func(obs ReceiverRecoveryObservation) {
						obsMu.Lock()
						observations = append(observations, obs)
						obsMu.Unlock()
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				set, err := recovery.Join(context.Background(), ReceiverConfig{
					Receiver: liveshare.ReceiverConfig{
						Capacity:   testReceiveCapacity(t),
						Capability: sender.Capability(),
					},
					Dial: func(_ context.Context, cfg relayv2.ReceiverConfig) (ReceiverConnection, error) {
						if cfg.RelayBaseURL == relayB {
							relayBDials.Add(1)
							return nil, &relayv2.RelayError{Code: v2.ErrorNotFound}
						}
						if relayADials.Add(1) > 1 {
							return nil, errors.New("unexpected retry dial on primary relay")
						}
						sEnd, rEnd := newMemoryPipe()
						pipeMu.Lock()
						senderPipe, receiverPipe = sEnd, rEnd
						pipeMu.Unlock()
						senderDone.Go(func() {
							admission, admitErr := factory.AdmitChannel(context.Background(), sEnd)
							if admitErr != nil {
								_ = sEnd.Close()
								return
							}
							if admission.Session != nil {
								<-admission.Session.Done()
								admission.Session.Close()
							}
						})
						return &stagedCloseReceiverConnection{
							descriptor: sender.Registration().Descriptor,
							channel:    rEnd,
							endpoint:   endpointA,
							done:       make(chan struct{}),
						}, nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				var sendDone sync.WaitGroup
				defer func() {
					set.Close()
					sendDone.Wait()
					senderDone.Wait()
				}()
				runtime, _, err := set.WaitReady(context.Background())
				if err != nil {
					t.Fatal(err)
				}

				// Let relayB finish its first retry backoff and park inside waitForDemand.
				time.Sleep(2 * receiverRetryDelay)
				synctest.Wait()
				if relayBDials.Load() != 1 {
					t.Fatalf("relayB dials before teardown = %d, want 1", relayBDials.Load())
				}

				// Hold the sole lane's writer in channel.Send so sessioncore cannot reach
				// finishLane yet while teardown removes or marks the lane closing.
				sendEntered := make(chan struct{})
				releaseSend := make(chan struct{})
				released := false
				defer func() {
					if !released {
						close(releaseSend)
					}
				}()
				pipeMu.Lock()
				sPipe, rPipe := senderPipe, receiverPipe
				pipeMu.Unlock()
				rPipe.armSendGate(sendEntered, releaseSend)
				sendDone.Go(func() {
					_ = runtime.SendPeerPathControl(context.Background(), controlBody)
				})
				<-sendEntered

				detachDone := make(chan bool, 1)
				if tc.detachExplicitly {
					laneID, laneEpoch := runtime.LaneIdentity()
					go func() {
						detachDone <- runtime.DetachLane(session.LaneIdentity{ID: laneID, Epoch: laneEpoch})
					}()
				} else {
					if err := sPipe.Close(); err != nil {
						t.Fatal(err)
					}
				}
				synctest.Wait()

				if runtime.AttachedLanes() != tc.wantAttachedLanes || runtime.HasUsableLanes() ||
					runtime.Stopping() || runtime.PathsExhausted() {
					t.Fatalf("unexpected pre-finishLane teardown state: attached=%d usable=%v stopping=%v exhausted=%v",
						runtime.AttachedLanes(), runtime.HasUsableLanes(), runtime.Stopping(), runtime.PathsExhausted())
				}
				if err := set.waitForDemand(context.Background()); !errors.Is(err, session.ErrRuntimeClosed) {
					t.Fatalf("waitForDemand during teardown window = %v, want %v", err, session.ErrRuntimeClosed)
				}
				if err := set.attemptError(context.Background()); !errors.Is(err, session.ErrRuntimeClosed) {
					t.Fatalf("attemptError during teardown window = %v, want %v", err, session.ErrRuntimeClosed)
				}

				// Wake relayB's parked waitForDemand timer while teardown is still mid-flight.
				time.Sleep(2 * receiverRetryDelay)
				synctest.Wait()
				if relayBDials.Load() != 1 {
					t.Fatalf("parked relay worker dialed during teardown window: dials=%d", relayBDials.Load())
				}

				close(releaseSend)
				released = true
				if tc.detachExplicitly && !<-detachDone {
					t.Fatal("DetachLane returned false for active sole lane")
				}
				<-runtime.Done()
				if !runtime.PathsExhausted() {
					t.Fatal("sole lane teardown did not settle as PathsExhausted")
				}
				set.Close()
				sendDone.Wait()
				senderDone.Wait()

				if relayADials.Load() != 1 || relayBDials.Load() != 1 {
					t.Fatalf("unexpected dials: relayA=%d relayB=%d", relayADials.Load(), relayBDials.Load())
				}
				obsMu.Lock()
				defer obsMu.Unlock()
				var relayBConnecting int
				for _, obs := range observations {
					if obs.Endpoint == relayB && obs.Phase == ReceiverRecoveryConnecting {
						relayBConnecting++
					}
				}
				if relayBConnecting != 1 {
					t.Fatalf("relayB connecting observations = %d, want 1 (initial attempt only)", relayBConnecting)
				}
			})
		})
	}
}
