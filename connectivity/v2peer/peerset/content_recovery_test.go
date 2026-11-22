package peerset

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/connectivity/v2peer"
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
	for _, test := range []struct {
		name  string
		order attemptCloseOrder
	}{
		{name: "local_stop", order: closeSignalingFirst},
		{name: "recovery_restart", order: closeLaneFirst},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			path.closeAttempt(attempt, test.order)
			if lanes.Len() != 0 {
				t.Fatal("attachment won cancellation and escaped path ownership")
			}
		})
	}
}

func TestCloseAttemptOrdersSignalingAndLaneDetachByShutdownReason(t *testing.T) {
	for _, test := range []struct {
		name             string
		order            attemptCloseOrder
		wantLanesAtClose int
	}{
		{name: "local stop closes signaling before detaching lane", order: closeSignalingFirst, wantLanesAtClose: 1},
		{name: "recovery restart detaches broken lane before closing attempt", order: closeLaneFirst, wantLanesAtClose: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			lanes, err := transferlanes.New(transferlanes.Config{ProtocolSessionID: protocolsession.ProtocolSessionID{3}})
			if err != nil {
				t.Fatal(err)
			}
			defer lanes.Close()
			identity := session.LaneIdentity{ID: 2, Epoch: 1}
			if err := lanes.Add(transferlanes.Identity(identity), transferlanes.RouteDirect,
				recoveryBlockSource(func(context.Context, transferlanes.Demand) (records.BlockRecord, error) {
					return records.BlockRecord{}, nil
				})); err != nil {
				t.Fatal(err)
			}
			path := &Path{config: PathConfig{Content: recoveryLaneOwner{lanes}}}
			attempt := &recoveringAttempt{fakeAttempt: newAttempt(), identity: identity}
			lanesDuringClose := -1
			attempt.onClose = func() {
				lanesDuringClose = lanes.Len()
			}
			path.closeAttempt(attempt, test.order)
			if lanesDuringClose != test.wantLanesAtClose {
				t.Fatalf("admitted lane count during attempt.Close = %d, want %d", lanesDuringClose, test.wantLanesAtClose)
			}
			if lanes.Len() != 0 {
				t.Fatalf("admitted lane count after closeAttempt = %d, want 0", lanes.Len())
			}
		})
	}
}

type pathLanesTestSession struct {
	v2peer.ReceiverLaneSession
	recoveryLaneOwner
}

func TestPathLanesAdaptsContentOwnerToLaneLifetimeSession(t *testing.T) {
	lanes, err := transferlanes.New(transferlanes.Config{ProtocolSessionID: protocolsession.ProtocolSessionID{9}})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	identity := session.LaneIdentity{ID: 4, Epoch: 2}
	if err := lanes.Add(transferlanes.Identity(identity), transferlanes.RouteDirect,
		recoveryBlockSource(func(context.Context, transferlanes.Demand) (records.BlockRecord, error) {
			return records.BlockRecord{}, nil
		})); err != nil {
		t.Fatal(err)
	}
	sessionID := protocolsession.ProtocolSessionID{9}
	adapted := pathLanes(pathLanesTestSession{recoveryLaneOwner: recoveryLaneOwner{lanes}}, sessionID)
	lifetime, ok := adapted.(v2peer.ReceiverLaneLifetimeSession)
	if !ok {
		t.Fatalf("pathLanes returned %T without ReceiverLaneLifetimeSession", adapted)
	}
	if idCarrier, ok := adapted.(interface {
		ProtocolSessionID() protocolsession.ProtocolSessionID
	}); !ok || idCarrier.ProtocolSessionID() != sessionID {
		t.Fatalf("adapted session ID preserved=%v", ok)
	}
	if !lifetime.DetachAdmittedLane(identity) || lanes.Len() != 0 {
		t.Fatalf("DetachAdmittedLane failed to detach lane; remaining=%d", lanes.Len())
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
