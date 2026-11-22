package nativepeer

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/pionpeer"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type mappedGateway struct{ deletes atomic.Int32 }

func (g *mappedGateway) Create(_ context.Context, request reachability.Request) (reachability.Lease, error) {
	return reachability.Lease{External: netip.MustParseAddrPort("8.8.8.8:4242"), Lifetime: time.Minute, GatewayID: "fake", ResourceID: "mapping", Kind: "pcp"}, nil
}
func (g *mappedGateway) Renew(ctx context.Context, request reachability.Request, _ reachability.Lease) (reachability.Lease, error) {
	return g.Create(ctx, request)
}
func (g *mappedGateway) Delete(context.Context, reachability.Request, reachability.Lease) error {
	g.deletes.Add(1)
	return nil
}
func TestMappingFactsAreFrozenOnlyIntoFreshAttemptAndRetainedOnlyForSelectedLane(t *testing.T) {
	h := newHarness(t)
	gateway := &mappedGateway{}
	h.native.config.Reachability = reachability.New(reachability.Config{Now: func() time.Time { return h.now }, Gateways: []reachability.Gateway{gateway}})
	req := request(1)
	changes, _ := h.native.SetDemand(req.ProtocolSessionID, req.Binding.PeerPathID, true, false)
	_, _ = h.native.NewPeerConnection(context.Background(), req)
	if len(h.requests[0].MappedEndpoints) != 0 {
		t.Fatal("mapping appeared before gateway response")
	}
	h.native.Maintain(context.Background())
	h.now = h.now.Add(3 * time.Second)
	h.native.Maintain(context.Background())
	select {
	case change := <-changes:
		if !change.MappingReady {
			t.Fatal(change)
		}
	default:
		t.Fatal("missing mapping readiness")
	}
	if len(h.requests[0].MappedEndpoints) != 0 {
		t.Fatal("late mapping mutated active snapshot")
	}
	_, _ = h.native.NewPeerConnection(context.Background(), request(2))
	second := h.requests[1]
	if len(second.MappedEndpoints) != 1 || second.MappedEndpoints[0].Local != second.SocketLease.Endpoints()[0] {
		t.Fatal(second.MappedEndpoints)
	}
	second.Observe(pionpeer.Event{Pair: &pionpeer.PairFacts{LocalAddress: "8.8.8.8", LocalPort: 4242, Protocol: "udp"}})
	h.native.SetAdmitted(req.ProtocolSessionID, req.Binding.PeerPathID, true)
	if len(h.native.config.Reachability.Facts()) != 1 {
		t.Fatal("admitted mapping lost its lease")
	}
	if err := h.native.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gateway.deletes.Load() != 1 {
		t.Fatal("final demand did not revoke exact lease", gateway.deletes.Load())
	}
	if _, err := h.native.NewPeerConnection(context.Background(), request(3)); err == nil {
		t.Fatal("closed owner reopened")
	}
	if err := h.native.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestSenderMaintenancePublishesOnlyNewLocalFacts(t *testing.T) {
	h := newHarness(t)
	session := [16]byte{1}
	pathID := v2signal.PeerPathID{2}
	h.native.flushSessionControls(context.Background(), session, nil)
	changes, _ := h.native.SetDemand(session, pathID, true, false)
	_ = changes
	path := h.native.paths[pathKey{session, pathID}]
	notify(path, Change{MappingReady: true})
	var sent []protocolsession.PeerPathControl
	send := func(_ context.Context, body []byte) error {
		control, err := protocolsession.DecodePeerPathControl(body)
		if err == nil {
			sent = append(sent, control)
		}
		return err
	}
	h.native.flushSessionControls(context.Background(), session, send)
	if len(sent) != 1 || sent[0].Kind != protocolsession.PeerPathMappingReady {
		t.Fatal(sent)
	}
	notify(path, Change{Remote: true, MappingReady: true})
	h.native.flushSessionControls(context.Background(), session, send)
	if len(sent) != 1 {
		t.Fatal("remote mapping echoed")
	}
	notify(path, Change{NetworkGenerationID: 2})
	h.native.flushSessionControls(context.Background(), session, send)
	if len(sent) != 2 || sent[1].Kind != protocolsession.PeerPathNetworkChanged {
		t.Fatal(sent)
	}
	notify(path, Change{MappingReady: true})
	notify(path, Change{NetworkGenerationID: 4})
	h.native.flushSessionControls(context.Background(), session, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.native.RunSession(ctx, session, send)
	h.native.SetAdmitted(session, v2signal.PeerPathID{255}, true)
	if h.native.Retired(session, pathID) {
		t.Fatal("live path retired")
	}
	if _, ok := h.native.ApplyControl(session, controlBytes(t, 1, protocolsession.PeerPathRevoke, 0)); !ok {
		t.Fatal("revoke")
	}
	if !h.native.Retired(session, pathID) {
		t.Fatal("retirement lost")
	}
	if _, ok := h.native.ApplyControl(session, controlBytes(t, 2, protocolsession.PeerPathDemand, ControlLifetime)); ok {
		t.Fatal("revoked path resurrected")
	}
}

type countingNetworkSource struct {
	mu      sync.Mutex
	calls   atomic.Int64
	state   networkstate.State
	entered chan struct{}
	block   chan struct{}
}

func (s *countingNetworkSource) Snapshot(ctx context.Context) (networkstate.State, error) {
	s.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return networkstate.State{}, err
	}
	s.mu.Lock()
	entered, block := s.entered, s.block
	s.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return networkstate.State{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}

func TestRunSessionSharesWatcherAndPublishesNetworkChangesImmediately(t *testing.T) {
	now := time.Unix(500, 0)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

	source := &countingNetworkSource{state: networkstate.State{Addresses: []networkstate.Address{{IP: netip.MustParseAddr("192.0.2.1"), InterfaceIndex: 1}}}}
	watcher := networkstate.NewWatcher(networkstate.WatcherConfig{Source: source, Debounce: time.Nanosecond, Interval: time.Hour, Now: clock})
	defer watcher.Close()

	native := newTestConnectivity(t, Config{Monitor: watcher, Now: clock, Reachability: reachability.New(reachability.Config{Now: clock})})
	defer native.Close(context.Background())

	const sessions = 4
	ctx, cancel := context.WithCancel(context.Background())
	sent := make([]chan protocolsession.PeerPathControl, sessions)
	var wg sync.WaitGroup
	for i := range sessions {
		session := [16]byte{byte(i + 1)}
		pathID := v2signal.PeerPathID{byte(i + 10)}
		if _, err := native.SetDemand(session, pathID, true, false); err != nil {
			t.Fatal(err)
		}
		ch := make(chan protocolsession.PeerPathControl, 4)
		sent[i] = ch
		wg.Go(func() {
			native.RunSession(ctx, session, func(_ context.Context, body []byte) error {
				control, err := protocolsession.DecodePeerPathControl(body)
				if err == nil {
					ch <- control
				}
				return err
			})
		})
	}
	// Wait until the shared watcher establishes baseline generation 1, then ensure
	// repeated Maintain calls reuse the cached snapshot without extra OS queries.
	if _, err := watcher.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		native.Maintain(context.Background())
	}
	if calls := source.calls.Load(); calls != 1 {
		t.Fatalf("shared watcher polls across %d sessions = %d, want 1", sessions, calls)
	}

	source.mu.Lock()
	source.state.ResumeSequence++
	source.mu.Unlock()
	if _, _, err := watcher.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	advance(time.Millisecond)
	if _, changed, err := watcher.Step(context.Background()); err != nil || !changed {
		t.Fatalf("network change step = (%t, %v)", changed, err)
	}
	for i, ch := range sent {
		select {
		case control := <-ch:
			if control.Kind != protocolsession.PeerPathNetworkChanged {
				t.Fatalf("session %d control = %+v", i, control)
			}
		case <-time.After(time.Second):
			t.Fatalf("session %d did not immediately emit network change", i)
		}
	}
	cancel()
	wg.Wait()
}

func TestRetainMaintenanceSyncsMissedNetworkGenerationOnSubscribe(t *testing.T) {
	now := time.Unix(600, 0)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

	source := &countingNetworkSource{state: networkstate.State{Addresses: []networkstate.Address{{IP: netip.MustParseAddr("192.0.2.1"), InterfaceIndex: 1}}}}
	watcher := networkstate.NewWatcher(networkstate.WatcherConfig{Source: source, Debounce: time.Nanosecond, Interval: time.Hour, Now: clock})
	defer watcher.Close()

	if snap, err := watcher.Snapshot(context.Background()); err != nil || snap.GenerationID() != 1 {
		t.Fatalf("baseline snapshot = (%d, %v)", snap.GenerationID(), err)
	}

	native := newTestConnectivity(t, Config{Monitor: watcher, Now: clock, Reachability: reachability.New(reachability.Config{Now: clock})})
	defer native.Close(context.Background())

	session := [16]byte{7}
	pathID := v2signal.PeerPathID{8}
	// Use content=false with no lease so MaintenanceInterval ticks would not
	// invoke Maintain; catching up must come from post-subscribe snapshot sync.
	changes, err := native.SetDemand(session, pathID, false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Cold-start RetainMaintenance adopts baseline generation 1 without emitting
	// a spurious network-change notice.
	releaseBaseline := native.RetainMaintenance()
	deadline := time.Now().Add(250 * time.Millisecond)
	for native.NetworkGeneration() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := native.NetworkGeneration(); got != 1 {
		releaseBaseline()
		t.Fatalf("baseline generation = %d, want 1", got)
	}
	releaseBaseline()
	select {
	case change := <-changes:
		t.Fatalf("cold-start baseline emitted unexpected change: %+v", change)
	default:
	}

	// Advance the shared watcher while no maintainer is subscribed, then retain
	// maintenance again and verify the missed transition is delivered immediately.
	source.mu.Lock()
	source.state.ResumeSequence++
	source.mu.Unlock()
	if _, _, err := watcher.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	advance(time.Millisecond)
	if snap, changed, err := watcher.Step(context.Background()); err != nil || !changed || snap.GenerationID() != 2 {
		t.Fatalf("network change step = (%d, %t, %v)", snap.GenerationID(), changed, err)
	}

	release := native.RetainMaintenance()
	defer release()
	select {
	case change := <-changes:
		if change.NetworkGenerationID != 2 {
			t.Fatalf("caught-up change = %+v, want generation 2", change)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatalf("RetainMaintenance did not immediately sync missed generation 2 (current=%d)", native.NetworkGeneration())
	}
}
func TestIdleUsesOnlyBoundedPreviouslySelectedProfileAndOwnerCloseJoinsGenerationCleanup(t *testing.T) {
	h := newHarness(t)
	h.native.Idle(context.Background(), [16]byte{1}, v2signal.PeerPathID{2}, h.now)
	_, _ = h.native.NewPeerConnection(context.Background(), request(1))
	path := h.native.paths[pathKey{[16]byte{1}, v2signal.PeerPathID{2}}]
	path.lastURLs = []string{"bad-url", "stun:127.0.0.1:zero", "stun:127.0.0.1:0", "stun:127.0.0.1:1"}
	h.native.Idle(context.Background(), [16]byte{1}, v2signal.PeerPathID{2}, h.now)
	if err := h.native.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	automatic := newTestConnectivity(t, Config{Monitor: h.monitor, Now: func() time.Time { return h.now }, Pool: h.native.config.Pool, Connect: func(pion.Configuration, pionpeer.AttemptConfig) (*pionpeer.Connection, error) {
		return nil, context.Canceled
	}})
	_, _ = automatic.NewPeerConnection(context.Background(), request(1))
	h.monitor.state.ResumeSequence++
	_, _ = automatic.NewPeerConnection(context.Background(), request(2))
	if err := automatic.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	defaults := newTestConnectivity(t, Config{})
	if err := defaults.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareAttemptCancelsWhileSharedWatcherSampleBlocks(t *testing.T) {
	now := time.Unix(700, 0)
	clock := func() time.Time { return now }
	block := make(chan struct{})
	source := &countingNetworkSource{
		state:   networkstate.State{Addresses: []networkstate.Address{{IP: netip.MustParseAddr("192.0.2.1"), InterfaceIndex: 1}}},
		entered: make(chan struct{}, 1),
		block:   block,
	}
	watcher := networkstate.NewWatcher(networkstate.WatcherConfig{Source: source, Debounce: time.Nanosecond, Interval: time.Hour, Now: clock})
	defer watcher.Close()

	native := newTestConnectivity(t, Config{Monitor: watcher, Now: clock, Reachability: reachability.New(reachability.Config{Now: clock})})
	defer native.Close(context.Background())

	leaderDone := make(chan error, 1)
	go func() {
		prepared, err := native.PrepareAttempt(context.Background(), request(1))
		if prepared != nil {
			prepared.Close()
		}
		leaderDone <- err
	}()
	<-source.entered

	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelWaiter()
	waiterDone := make(chan error, 1)
	go func() {
		prepared, err := native.PrepareAttempt(waiterCtx, request(2))
		if prepared != nil {
			prepared.Close()
		}
		waiterDone <- err
	}()

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			close(block)
			<-leaderDone
			t.Fatalf("second PrepareAttempt err = %v, want DeadlineExceeded", err)
		}
	case <-time.After(time.Second):
		close(block)
		<-leaderDone
		t.Fatal("second PrepareAttempt blocked past deadline while shared sample was stuck")
	}

	close(block)
	if err := <-leaderDone; err != nil {
		t.Fatalf("first PrepareAttempt err = %v", err)
	}
	if calls := source.calls.Load(); calls != 1 {
		t.Fatalf("source calls = %d, want 1", calls)
	}
}

type blockingGateway struct {
	entered  chan struct{}
	canceled chan error
	release  chan struct{}
	enter    sync.Once
	unblock  sync.Once
}

func newBlockingGateway() *blockingGateway {
	return &blockingGateway{
		entered:  make(chan struct{}),
		canceled: make(chan error, 1),
		release:  make(chan struct{}),
	}
}

func (g *blockingGateway) unblockAll() { g.unblock.Do(func() { close(g.release) }) }

func (g *blockingGateway) Create(ctx context.Context, _ reachability.Request) (reachability.Lease, error) {
	g.enter.Do(func() { close(g.entered) })
	<-ctx.Done()
	select {
	case g.canceled <- ctx.Err():
	default:
	}
	<-g.release
	return reachability.Lease{}, ctx.Err()
}

func (g *blockingGateway) Renew(ctx context.Context, request reachability.Request, _ reachability.Lease) (reachability.Lease, error) {
	return g.Create(ctx, request)
}

func (g *blockingGateway) Delete(context.Context, reachability.Request, reachability.Lease) error {
	return nil
}

func TestNetworkChangeCancelsInFlightAttemptWhileGatewayReconcileBlocks(t *testing.T) {
	now := time.Unix(700, 0)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

	source := &countingNetworkSource{state: networkstate.State{
		Addresses: []networkstate.Address{{IP: netip.MustParseAddr("192.0.2.1"), InterfaceIndex: 1}},
	}}
	watcher := networkstate.NewWatcher(networkstate.WatcherConfig{
		Source: source, Debounce: time.Nanosecond, Interval: time.Hour, Now: clock,
	})
	defer watcher.Close()
	if snap, err := watcher.Snapshot(context.Background()); err != nil || snap.GenerationID() != 1 {
		t.Fatalf("baseline snapshot = (%d, %v)", snap.GenerationID(), err)
	}

	gw := newBlockingGateway()
	defer gw.unblockAll()
	authority := reachability.New(reachability.Config{
		Now:              clock,
		OperationTimeout: reachability.DefaultOperationTimeout,
		Gateways:         []reachability.Gateway{gw},
	})
	sockets := testSocketAuthority(t, 0)
	defer func() { _ = sockets.Close() }()
	native := newTestConnectivity(t, Config{
		Monitor:      watcher,
		Sockets:      sockets,
		Now:          clock,
		Reachability: authority,
		Connect: func(pion.Configuration, pionpeer.AttemptConfig) (*pionpeer.Connection, error) {
			return nil, errors.New("capture")
		},
	})
	defer func() {
		gw.unblockAll()
		_ = native.Close(context.Background())
	}()

	req1 := request(1)
	if _, err := native.SetDemand(req1.ProtocolSessionID, req1.Binding.PeerPathID, true, false); err != nil {
		t.Fatal(err)
	}
	// Install the generation-1 socket lease on the path, then seed reachability
	// head-start and keep a generation-1 attempt active for cancellation checks.
	_, _ = native.NewPeerConnection(context.Background(), req1)
	native.Maintain(context.Background())
	advance(reachability.DefaultHeadStart + time.Millisecond)

	attempt, err := native.PrepareAttempt(context.Background(), request(2))
	if err != nil {
		t.Fatal(err)
	}
	defer attempt.Close()

	ctx, cancel := context.WithCancel(context.Background())
	sent := make(chan protocolsession.PeerPathControl, 4)
	var sessionWG sync.WaitGroup
	sessionWG.Go(func() {
		native.RunSession(ctx, req1.ProtocolSessionID, func(_ context.Context, body []byte) error {
			control, decodeErr := protocolsession.DecodePeerPathControl(body)
			if decodeErr == nil {
				sent <- control
			}
			return decodeErr
		})
	})
	defer func() {
		cancel()
		gw.unblockAll()
		sessionWG.Wait()
	}()

	// Trigger gateway reconciliation immediately without waiting for the 1s tick.
	native.mu.Lock()
	native.wakeMaintenanceLocked()
	native.mu.Unlock()

	select {
	case <-gw.entered:
	case <-time.After(time.Second):
		t.Fatal("gateway reconcile did not enter blocking Create")
	}

	// Publish generation 2 while gateway.Create is still held open before release.
	source.mu.Lock()
	source.state.ResumeSequence++
	source.mu.Unlock()
	if _, _, err := watcher.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	advance(time.Millisecond)
	if snap, changed, err := watcher.Step(context.Background()); err != nil || !changed || snap.GenerationID() != 2 {
		t.Fatalf("network change step = (%d, %t, %v)", snap.GenerationID(), changed, err)
	}

	// All assertions below must complete while gateway.Create is still blocked on
	// gw.release, proving maintenance does not wait for the 2s OperationTimeout.
	select {
	case <-attempt.ctx.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("old ICE attempt was delayed behind blocked gateway reconcile")
	}
	select {
	case err := <-gw.canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("gateway cancel err = %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("blocked gateway operation was not canceled immediately by authority retirement")
	}
	select {
	case control := <-sent:
		if control.Kind != protocolsession.PeerPathNetworkChanged {
			t.Fatalf("emitted control = %+v, want PeerPathNetworkChanged", control)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("session control notice was delayed behind blocked gateway reconcile")
	}

	gw.unblockAll()
}

func TestRetainMaintenanceReconcilesMappingAsynchronously(t *testing.T) {
	now := time.Unix(800, 0)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

	source := &countingNetworkSource{state: networkstate.State{
		Addresses: []networkstate.Address{{IP: netip.MustParseAddr("192.0.2.1"), InterfaceIndex: 1}},
	}}
	watcher := networkstate.NewWatcher(networkstate.WatcherConfig{
		Source: source, Debounce: time.Nanosecond, Interval: time.Hour, Now: clock,
	})
	defer watcher.Close()
	if _, err := watcher.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}

	gw := &mappedGateway{}
	sockets := testSocketAuthority(t, 0)
	defer func() { _ = sockets.Close() }()
	native := newTestConnectivity(t, Config{
		Monitor:      watcher,
		Sockets:      sockets,
		Now:          clock,
		Reachability: reachability.New(reachability.Config{Now: clock, Gateways: []reachability.Gateway{gw}}),
		Connect: func(pion.Configuration, pionpeer.AttemptConfig) (*pionpeer.Connection, error) {
			return nil, errors.New("capture")
		},
	})
	defer func() { _ = native.Close(context.Background()) }()

	req := request(1)
	changes, err := native.SetDemand(req.ProtocolSessionID, req.Binding.PeerPathID, true, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = native.NewPeerConnection(context.Background(), req)
	native.Maintain(context.Background())
	advance(reachability.DefaultHeadStart + time.Millisecond)

	release := native.RetainMaintenance()
	defer release()
	native.mu.Lock()
	native.wakeMaintenanceLocked()
	native.mu.Unlock()

	select {
	case change := <-changes:
		if !change.MappingReady || change.NetworkGenerationID != 1 {
			t.Fatalf("mapping change = %+v, want MappingReady on generation 1", change)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("async reconciler did not publish MappingReady")
	}
}
