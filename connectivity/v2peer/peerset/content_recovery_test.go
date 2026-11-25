package peerset

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/transfer/fault"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

type recoveringAttempt struct {
	*fakeAttempt
	identity session.LaneIdentity
	retire   sync.Once
	onClose  func()
}

func (attempt *recoveringAttempt) Lane() (session.LaneIdentity, bool) {
	return attempt.identity, attempt.identity.ID != 0
}

func (attempt *recoveringAttempt) Close() error {
	attempt.retire.Do(func() {
		if attempt.onClose != nil {
			attempt.onClose()
		}
		_ = attempt.fakeAttempt.Close()
	})
	return nil
}

type recoveryLaneOwner struct{ lanes *transferlanes.Set }

func (owner recoveryLaneOwner) LaneSet() *transferlanes.Set { return owner.lanes }
func (owner recoveryLaneOwner) DetachLane(identity session.LaneIdentity) bool {
	return owner.lanes.Remove(transferlanes.Identity(identity))
}

type recoveryBlockSource func(context.Context, transferlanes.Demand) (records.BlockRecord, error)

func (source recoveryBlockSource) FetchBlock(ctx context.Context, demand transferlanes.Demand, _ func(uint64)) (records.BlockRecord, error) {
	return source(ctx, demand)
}

func TestAttemptClosureRetiresAdmissionThatWonCancellationRace(t *testing.T) {
	lanes, err := transferlanes.New(transferlanes.Config{ProtocolSessionID: protocolsession.ProtocolSessionID{2}})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	path := &Path{config: PathConfig{Content: recoveryLaneOwner{lanes}}}
	attempt := &recoveringAttempt{fakeAttempt: newAttempt()}
	attempt.onClose = func() {
		attempt.identity = session.LaneIdentity{ID: 2, Epoch: 1}
		err := lanes.Add(transferlanes.Identity(attempt.identity), transferlanes.RouteDirect,
			recoveryBlockSource(func(context.Context, transferlanes.Demand) (records.BlockRecord, error) {
				t.Error("late admission served content after its owner stopped")
				return records.BlockRecord{}, nil
			}))
		if err != nil {
			t.Fatal(err)
		}
	}
	path.closeAttempt(attempt)
	if lanes.Len() != 0 {
		t.Fatal("attachment won cancellation and escaped path ownership")
	}
}

func TestExhaustedContentRecoversWithoutThroughputGrowth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sessionID := protocolsession.ProtocolSessionID{1}
		lanes, err := transferlanes.New(transferlanes.Config{
			ProtocolSessionID: sessionID, ContentRoutePolicy: transferlanes.ContentRouteDirectOnly,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer lanes.Close()
		geometry, err := content.NewFileGeometry(uint64(catalog.MinChunkSize), catalog.MinChunkSize)
		if err != nil {
			t.Fatal(err)
		}
		descriptor, err := content.NewFileRevisionDescriptor(catalog.ShareInstance{1}, catalog.FileID{2},
			content.FileRevision{3}, geometry, catalog.ModifiedTime{})
		if err != nil {
			t.Fatal(err)
		}
		record, err := records.NewBlockRecord(descriptor, 0, bytes.Repeat([]byte{7}, int(catalog.MinChunkSize)))
		if err != nil {
			t.Fatal(err)
		}
		owner, err := newTestPeerSet(t, Config{})
		if err != nil {
			t.Fatal(err)
		}
		bindings := make(chan v2signal.Binding, 3)
		var path *Path
		path, err = owner.Open(context.Background(), PathConfig{SessionID: sessionID, Demand: ContentDemand, Content: recoveryLaneOwner{lanes},
			Start: func(_ context.Context, binding v2signal.Binding) (Attempt, error) {
				if binding.AttemptSequence > 1 && lanes.Len() != 0 {
					t.Error("replacement began before retiring the admitted data transport")
				}
				identity := session.LaneIdentity{ID: 2, Epoch: uint32(binding.AttemptSequence)}
				attempt := &recoveringAttempt{fakeAttempt: newAttempt(), identity: identity}
				attempt.onClose = func() {
					if path.recoverContentGeneration(transferlanes.Identity(identity)) {
						t.Error("closing generation retained recovery authority")
					}
				}
				source := recoveryBlockSource(func(context.Context, transferlanes.Demand) (records.BlockRecord, error) {
					if identity.Epoch == 1 {
						value, _ := fault.NewSession(fault.ScopeOutputPause, fault.SessionTransport)
						return records.BlockRecord{}, transferlanes.NewDemandReassignableAfterRetirement(
							fault.Wrap(value, errors.New("fragment inactivity")))
					}
					return record, nil
				})
				if err := lanes.Add(transferlanes.Identity(identity), transferlanes.RouteDirect, source); err != nil {
					return nil, err
				}
				close(attempt.ready)
				bindings <- binding
				return attempt, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer path.Close()
		<-path.Ready()
		initial := <-bindings
		budget := path.budget()
		done := make(chan error, 1)
		go func() {
			block, err := lanes.Fetch(context.Background(), transferlanes.Request{
				Demand: transferlanes.Demand{LeaseID: content.LeaseID{4}, Descriptor: descriptor},
				Validate: func(actual records.BlockRecord) error {
					if actual.Descriptor() != descriptor || actual.LocalBlockIndex() != 0 {
						return errors.New("incorrect immutable content")
					}
					return nil
				},
			})
			if err == nil && !bytes.Equal(block.Data(), record.Data()) {
				t.Error("recovery changed delivered content")
			}
			done <- err
		}()
		synctest.Wait()
		if demand := lanes.SchedulingDemand(); demand.UsefulBytes != 0 || demand.OutputBytes != 0 {
			t.Fatalf("fixture must recover before any throughput evidence: %+v", demand)
		}
		session := &ReceiverSession{owner: owner}
		session.recoverContentDemand(lanes)
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("exhausted request did not resume on its replacement")
		}
		replacement := <-bindings
		if replacement.PeerPathID != initial.PeerPathID || replacement.AttemptSequence <= initial.AttemptSequence ||
			replacement.Purpose != v2signal.Recovery || path.budget() != budget {
			t.Fatalf("recovery changed path or budget ownership: initial=%+v replacement=%+v", initial, replacement)
		}
		if path.recoverContentGeneration(transferlanes.Identity{ID: 2, Epoch: 1}) {
			t.Fatal("stale failure restarted a healthy replacement")
		}
		session.recoverContentDemand(lanes)
		synctest.Wait()
		select {
		case binding := <-bindings:
			t.Fatalf("completed recovery started an extra attempt: %+v", binding)
		default:
		}
	})
}

func TestExhaustedTURNContentRecoversAcrossRouteBlockedFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sessionID := protocolsession.ProtocolSessionID{9}
		laneSet, err := transferlanes.New(transferlanes.Config{
			ProtocolSessionID:  sessionID,
			ContentRoutePolicy: transferlanes.ContentRouteAll,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer laneSet.Close()
		// An active direct path holds RouteSupply even while direct P2P cannot connect,
		// parking exhausted reads in CapacityRoute with zero NetworkWaitingBytes.
		supply, err := laneSet.ExpectRoute(transferlanes.RouteDirect)
		if err != nil {
			t.Fatal(err)
		}
		defer supply.Close()

		geometry, err := content.NewFileGeometry(uint64(catalog.MinChunkSize), catalog.MinChunkSize)
		if err != nil {
			t.Fatal(err)
		}
		descriptor, err := content.NewFileRevisionDescriptor(catalog.ShareInstance{1}, catalog.FileID{2},
			content.FileRevision{3}, geometry, catalog.ModifiedTime{})
		if err != nil {
			t.Fatal(err)
		}
		record, err := records.NewBlockRecord(descriptor, 0, bytes.Repeat([]byte{9}, int(catalog.MinChunkSize)))
		if err != nil {
			t.Fatal(err)
		}

		owner, err := newTestPeerSet(t, Config{})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		runtime := &turnTestRuntime{
			lanes:    make(map[session.LaneIdentity]*turnTestLane),
			requests: make(chan protocolsession.TURNDemand, 8),
		}
		receiver := &ReceiverSession{owner: owner, ctx: ctx, config: ReceiverConfig{Demand: ContentDemand}}
		manager := newTURNManager(receiver, runtime, laneSet)
		receiver.turn = manager
		defer func() { cancel(); manager.close(); receiver.work.Wait() }()

		attempts := make(chan *testTURNAttempt, 4)
		var nextLaneID atomic.Uint32
		manager.negotiate = func(_ context.Context, binding v2signal.Binding, grant protocolsession.TURNGrant) (Attempt, error) {
			id := session.LaneIdentity{ID: nextLaneID.Add(1), Epoch: 1}
			attempt := &testTURNAttempt{fakeAttempt: newAttempt(), binding: binding, grant: grant, lane: id}
			attempts <- attempt
			return attempt, nil
		}

		admitTURN := func(a *testTURNAttempt, stall bool) {
			runtime.attach(a.lane)
			source := recoveryBlockSource(func(context.Context, transferlanes.Demand) (records.BlockRecord, error) {
				if stall {
					value, _ := fault.NewSession(fault.ScopeOutputPause, fault.SessionTransport)
					return records.BlockRecord{}, transferlanes.NewDemandReassignableAfterRetirement(
						fault.Wrap(value, errors.New("fragment inactivity")))
				}
				return record, nil
			})
			if err := laneSet.Add(transferlanes.Identity(a.lane), transferlanes.RouteTURN, source); err != nil {
				t.Fatal(err)
			}
			close(a.ready)
			synctest.Wait()
		}

		origin := owner.config.Clock.Now()
		manager.acceptService(origin, turnGrant("home"), false)
		synctest.Wait()
		first := receive(t, attempts)
		admitTURN(first, true)
		manager.step(origin, true, laneSet.SchedulingDemand(), nil)
		synctest.Wait()

		done := make(chan error, 1)
		go func() {
			block, fetchErr := laneSet.Fetch(t.Context(), transferlanes.Request{
				Demand: transferlanes.Demand{LeaseID: content.LeaseID{4}, Descriptor: descriptor},
				Validate: func(actual records.BlockRecord) error {
					if actual.Descriptor() != descriptor || actual.LocalBlockIndex() != 0 {
						return errors.New("incorrect immutable content")
					}
					return nil
				},
			})
			if fetchErr == nil && !bytes.Equal(block.Data(), record.Data()) {
				t.Error("TURN recovery changed delivered content")
			}
			done <- fetchErr
		}()
		synctest.Wait()

		sched := laneSet.SchedulingDemand()
		if sched.NetworkWaitingBytes != 0 || sched.UnfinishedBytes != 0 || sched.QueuedBytes == 0 {
			t.Fatalf("expected CapacityRoute demand shape, got %+v", sched)
		}

		// Step 1: ReceiverSession forwards RecoveryDemand from the stalled TURN lane.
		failed := receiver.recoverContentDemand(laneSet)
		receiver.observeTURNDemand(sched, failed)
		synctest.Wait()

		// Step 2: Advance past turnBackoff[0]; TURN manager requests a replacement grant.
		time.Sleep(turnBackoff[0])
		failed = receiver.recoverContentDemand(laneSet)
		receiver.observeTURNDemand(laneSet.SchedulingDemand(), failed)
		synctest.Wait()

		req := receive(t, runtime.requests)
		if req.Mode != protocolsession.TURNAll || len(req.ServiceIDs) != 1 || req.ServiceIDs[0] != "home" {
			t.Fatalf("unexpected TURN recovery request: %+v", req)
		}
		replyBody, err := protocolsession.EncodeTURNGrants(protocolsession.TURNGrants{
			RequestID: req.RequestID, HasFallback: true, Status: protocolsession.TURNOK,
			Grants: []protocolsession.TURNGrant{turnGrant("home")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.handler(t.Context(), replyBody); err != nil {
			t.Fatal(err)
		}

		failed = receiver.recoverContentDemand(laneSet)
		receiver.observeTURNDemand(laneSet.SchedulingDemand(), failed)
		synctest.Wait()
		second := receive(t, attempts)
		admitTURN(second, false)

		// Step 3: Replacement attaches, retires stalled predecessor, and completes the queued Fetch.
		failed = receiver.recoverContentDemand(laneSet)
		receiver.observeTURNDemand(laneSet.SchedulingDemand(), failed)
		synctest.Wait()

		select {
		case fetchErr := <-done:
			if fetchErr != nil {
				t.Fatal(fetchErr)
			}
		default:
			t.Fatal("route-blocked fetch did not resume on replacement TURN lane")
		}
		if _, usable := runtime.UsableLaneDone(first.lane); usable {
			t.Fatal("stalled TURN predecessor was not retired after handoff")
		}
		if !laneSet.HasDelivered(transferlanes.Identity(second.lane)) {
			t.Fatal("replacement TURN lane did not record authenticated delivery")
		}

		// Step 4: Next maintenance tick observes delivery and resets retry budget.
		receiver.observeTURNDemand(laneSet.SchedulingDemand(), receiver.recoverContentDemand(laneSet))
		synctest.Wait()
		if svc := manager.services["home"]; svc == nil || svc.retry.failures != 0 || svc.awaitingDelivery {
			t.Fatalf("healthy replacement did not clear stall retry debt: %+v", svc)
		}
	})
}
