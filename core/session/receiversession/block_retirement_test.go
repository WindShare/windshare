package receiversession

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestBlockRecoveredWhenRetiredCarrierLosesAcceptedFragments(t *testing.T) {
	synctest.Test(t, testBlockRecoveredWhenRetiredCarrierLosesAcceptedFragments)
}

func testBlockRecoveredWhenRetiredCarrierLosesAcceptedFragments(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	opened, err := receiver.OpenRevision(t.Context(), fixture.fileID)
	if err != nil {
		t.Fatal(err)
	}
	_, lostCarrier, _, _ := attachObservedLane(t, fixture.senderFactory, receiver, 0)
	attachObservedLane(t, fixture.senderFactory, receiver, 0)
	initialID, initialEpoch := receiver.LaneIdentity()
	initial := session.LaneIdentity{ID: initialID, Epoch: initialEpoch}
	if !receiver.DetachLane(initial) || !sender.DetachLane(initial) {
		t.Fatal("initial carrier did not detach")
	}
	var lost atomic.Int32
	lostCarrier.gateMu.Lock()
	lostCarrier.loseAndClose = &lost
	lostCarrier.gateMu.Unlock()
	output := make([]byte, len(fixture.fileData))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err = receiver.BlockBroker().ReadRange(ctx, opened.LeaseID, opened.Descriptor,
		content.Range{Offset: 0, End: uint64(len(output))},
		transfer.RangeSinkFunc(func(_ context.Context, offset uint64, data []byte) error {
			copy(output[offset:], data)
			return nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if lost.Load() < 2 {
		t.Fatal("did not exercise accepted loss followed by carrier failure")
	}
	if !bytes.Equal(output, fixture.fileData) {
		t.Fatal("recovery changed authenticated output")
	}
	// Recovery may finish through a rescue before the losing RPC processes its
	// cancellation. Check ownership after runnable cleanup settles, without
	// advancing a timeout or closing the session to revoke leaked authority.
	synctest.Wait()
	trackedRequests(receiver.rpc).mu.Lock()
	calls := len(trackedRequests(receiver.rpc).calls)
	trackedRequests(receiver.rpc).mu.Unlock()
	assertRequestsRetired(t, receiver.rpc)
	records := receiver.assembler.ActiveRecords()
	if calls != 0 || records != 0 {
		t.Fatalf("recovery retained calls=%d records=%d", calls, records)
	}
}

func TestMissingBlockCompletionRequiresAuthenticatedCarrierMigration(t *testing.T) {
	for _, test := range []struct {
		name      string
		migrated  bool
		count     uint32
		wantRetry bool
	}{
		{"same carrier", false, 1, false},
		{"migrated carrier", true, 1, true},
		{"malformed migrated count", true, 2, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newBlockProtocolFixture(t, true)
				done := make(chan error, 1)
				go func() { _, err := fixture.lane.FetchBlock(t.Context(), fixture.demand, nil); done <- err }()
				synctest.Wait()
				call := onlyActiveCall(t, fixture.lane.rpc)
				carrier := fixture.lane.identity
				if test.migrated {
					carrier.ID++
					carrier.Epoch = 1
				}
				body, err := contentflow.EncodeOperationComplete(test.count)
				if err != nil {
					t.Fatal(err)
				}
				signed, err := protocolsession.SignControlBody(
					ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
					protocolsession.ControlDomainOperation,
					protocolsession.ControlBinding{
						ShareInstance: fixture.demand.Descriptor.ShareInstance(), ProtocolSessionID: fixture.runtime.ProtocolSessionID(),
						LaneID: carrier.ID, LaneEpoch: carrier.Epoch, Direction: protocolsession.DirectionSenderToReceiver,
						Sequence: 1, MessageKind: protocolsession.MessageOperationComplete, OperationID: call.ID(), HasOperationID: true,
					}, body)
				if err != nil {
					t.Fatal(err)
				}
				message, err := protocolsession.NewMessage(protocolsession.MessageOperationComplete, new(call.ID()), signed)
				if err != nil {
					t.Fatal(err)
				}
				sendTestMessageOnLane(t, fixture.runtime, carrier, message)
				synctest.Wait()
				err = <-done
				retry := reflect.TypeOf(err) == reflect.TypeOf(transferlanes.NewDemandReassignableAfterRetirement(sessioncore.ErrLaneUnavailable))
				if retry != test.wantRetry {
					t.Fatalf("retry=%v want=%v: %v", retry, test.wantRetry, err)
				}
				if !test.wantRetry && !errors.Is(err, session.ErrOperationMissing) {
					t.Fatalf("protocol error changed: %v", err)
				}
				assertRequestsRetired(t, fixture.lane.rpc)
				cause := sessionobs.ProtocolOperationCauseProtocolFailure
				if test.wantRetry {
					cause = sessionobs.ProtocolOperationCauseLaneUnavailable
				}
				events := fixture.recorder.snapshot()
				if len(events) != 1 || events[0].Cause != cause {
					t.Fatalf("completion trace: %+v", events)
				}
			})
		})
	}
}
