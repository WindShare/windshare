package sessionruntime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type queueWaitRecordOpener struct {
	RecordOpener
	delay time.Duration
}

func (opener queueWaitRecordOpener) OpenBlock(descriptor content.FileRevisionDescriptor, index uint64, _ []byte) (records.BlockRecord, error) {
	if opener.delay > 0 {
		time.Sleep(opener.delay)
	}
	return records.NewBlockRecord(descriptor, index, []byte{42})
}

func TestBlockReceiverWaitsBehindProductiveRequests(t *testing.T) {
	for _, name := range []string{"new fragments", "duplicates do not extend waits", "slow local validation"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				duplicate := name == "duplicates do not extend waits"
				fixture := newBlockProtocolFixture(t, true)
				var validationDelay time.Duration
				if name == "slow local validation" {
					validationDelay = 20 * time.Second
				}
				fixture.lane.opener = queueWaitRecordOpener{delay: validationDelay}
				start := func() (*receiverBlockOperation, <-chan error) {
					operation, err := fixture.lane.beginBlockOperation(t.Context(), fixture.demand)
					if err != nil {
						t.Fatal(err)
					}
					done := make(chan error, 1)
					go func() {
						_, err := operation.receive(t.Context(), fixture.demand)
						done <- operation.finish(t.Context(), err)
					}()
					synctest.Wait()
					return operation, done
				}
				earlier, earlierDone := start()
				waiting, waitingDone := start()
				fragments, err := contentflow.FragmentRecord(earlier.call.id, make([]byte, 3*contentflow.MaxFragmentPayloadBytes))
				if err != nil {
					t.Fatal(err)
				}
				deliver := func(operation *receiverBlockOperation, message protocolsession.Message) {
					generation, _ := operation.call.operationAuthority()
					if err := operation.call.enqueue(operationResponse{message: message, generation: generation}); err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
				}
				for index := range 2 {
					time.Sleep(10 * time.Second)
					if duplicate {
						index = 0
					}
					deliver(earlier, fragments[index])
				}
				time.Sleep(5 * time.Second)
				synctest.Wait()
				if duplicate {
					if err := <-earlierDone; !errors.Is(err, contentflow.ErrFragmentInactivity) {
						t.Fatalf("duplicate revived assembly: %v", err)
					}
					if err := <-waitingDone; !errors.Is(err, contentflow.ErrBlockResponseInactivity) {
						t.Fatalf("duplicate prolonged queue: %v", err)
					}
					events := fixture.recorder.snapshot()
					for _, event := range events {
						if event.Cause != ProtocolOperationCauseDeadline || event.BlockWait.Waited != 25*time.Second {
							t.Fatalf("timeout evidence: %+v", event)
						}
					}
					return
				}
				select {
				case err := <-waitingDone:
					t.Fatalf("healthy queued request ended after 25 seconds: %v", err)
				default:
				}
				deliver(earlier, fragments[2])
				body, _ := contentflow.EncodeOperationComplete(1)
				complete := func(operation *receiverBlockOperation) {
					signed, err := protocolsession.SignControlBody(
						ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
						protocolsession.ControlDomainOperation,
						protocolsession.ControlBinding{
							ShareInstance:     fixture.demand.Descriptor.ShareInstance(),
							ProtocolSessionID: fixture.runtime.sessionID, LaneID: fixture.lane.identity.ID,
							LaneEpoch: fixture.lane.identity.Epoch, Direction: protocolsession.DirectionSenderToReceiver,
							Sequence: 1, MessageKind: protocolsession.MessageOperationComplete,
							OperationID: operation.call.id, HasOperationID: true,
						}, body)
					if err != nil {
						t.Fatal(err)
					}
					message, err := protocolsession.NewMessage(protocolsession.MessageOperationComplete, &operation.call.id, signed)
					if err != nil {
						t.Fatal(err)
					}
					deliver(operation, message)
				}
				complete(earlier)
				tail, _ := contentflow.FragmentRecord(waiting.call.id, []byte{1})
				deliver(waiting, tail[0])
				complete(waiting)
				for _, done := range []<-chan error{earlierDone, waitingDone} {
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				}
				for _, event := range fixture.recorder.snapshot() {
					if event.Stage != ProtocolOperationReceiverCompleted || event.Cause != ProtocolOperationCauseNone {
						t.Fatalf("queue extension poisoned operation outcome: %+v", event)
					}
				}
				if fixture.runtime.operations.ActiveCount() != 0 {
					t.Fatal("completed operations retained authority")
				}
			})
		})
	}
}

// Caller cancellation still outranks a pending queue allowance.
func TestQueuedBlockReceiverCancellationRemainsImmediate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newBlockProtocolFixture(t, true)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := fixture.lane.FetchBlock(ctx, fixture.demand); done <- err }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("caller cancellation lost: %v", err)
		}
		if fixture.runtime.operations.ActiveCount() != 0 {
			t.Fatal("canceled request retained authority")
		}
	})
}
