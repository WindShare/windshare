package receiversession

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestControlTransactionsSettleConfirmedFramesLostWithCarrier(t *testing.T) {
	for _, operation := range []string{"catalog", "open", "release"} {
		for _, direction := range []string{"request", "response"} {
			t.Run(operation+"/"+direction, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					fixture := newVerticalFixture(t)
					sender, receiver, relaySender, relayReceiver := connectObservedVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
					defer sender.Close()
					defer receiver.Close()
					var lease content.LeaseID
					if operation == "release" {
						opened, err := receiver.OpenRevision(t.Context(), fixture.fileID)
						if err != nil {
							t.Fatal(err)
						}
						lease = opened.LeaseID
					}
					_, peerSender, peerReceiver, _ := attachObservedLane(t, fixture.senderFactory, receiver, 0)
					peerSendsBefore := peerReceiver.sends.Load()
					requests := trackedRequests(receiver.rpc)
					requests.mu.Lock()
					issuedBefore := len(requests.issued)
					requests.mu.Unlock()
					var loss atomic.Int32
					lossCarrier := relaySender
					if direction == "request" {
						lossCarrier = relayReceiver
					}
					lossCarrier.gateMu.Lock()
					lossCarrier.loseAndClose = &loss
					lossCarrier.gateMu.Unlock()
					ctx, cancel := context.WithTimeout(t.Context(), time.Second)
					defer cancel()
					done := make(chan error, 1)
					go func() {
						var err error
						switch operation {
						case "catalog":
							_, err = receiver.Catalog().LoadDirectory(ctx, fixture.syntheticRoot)
						case "open":
							_, err = receiver.OpenRevision(ctx, fixture.fileID)
						case "release":
							err = receiver.ReleaseRevision(ctx, lease)
						}
						done <- err
					}()
					synctest.Wait()
					if loss.Load() != 1 {
						t.Fatalf("did not lose exactly one transport-confirmed response: %d", loss.Load())
					}
					oldCall := onlyActiveCall(t, receiver.rpc)
					oldID := oldCall.ID()
					if operation == "open" && direction == "response" {
						// A repeated OPEN creates another lease. The unknown, undelivered
						// lease remains bounded by the sender's existing lease lifetime.
						next, err := content.NewRevisionLease(id16[content.LeaseID](10), fixture.contentStore.descriptor,
							fixture.contentStore.lease.TTL(), fixture.contentStore.lease.RenewAfter())
						if err != nil {
							t.Fatal(err)
						}
						fixture.contentStore.lease = next
					}
					_ = relaySender.Close()
					_ = relayReceiver.Close()
					synctest.Wait()
					select {
					case err := <-done:
						if ctx.Err() != nil {
							t.Fatalf("carrier loss waited for the deadline: %v", ctx.Err())
						}
						if err != nil {
							t.Fatalf("repeatable control did not recover: %v", err)
						}
					default:
						t.Fatal("lost response remained pending despite a surviving lane")
					}
					if !oldCall.Operation().IsZero() {
						t.Fatal("retry retained the old operation's authority")
					}
					if peerReceiver.sends.Load() <= peerSendsBefore || peerSender.sends.Load() <= 1 {
						t.Fatal("replacement lane carried no recovered transaction")
					}
					if receiver.Err() != nil || receiver.ProtocolSessionID() != sender.ProtocolSessionID() {
						t.Fatal("carrier recovery replaced or failed the protocol session")
					}
					requests.mu.Lock()
					issued := append([]protocolsession.OperationID(nil), requests.issued[issuedBefore:]...)
					requests.mu.Unlock()
					if len(issued) != 2 || issued[0] != oldID || issued[1] == oldID {
						t.Fatalf("recovery did not issue one fresh operation: %v", issued)
					}
				})
			})
		}
	}
}

func TestControlTransactionsRecoverFromReplayWindowRetryAndCapAttempts(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver, _, _ := connectObservedVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	// Inject ErrOperationRetry on the first attempt of a repeatable control request and verify recovery.
	var remainingRetries atomic.Int32
	remainingRetries.Store(1)
	wrapped := &retryInjectingRequestClient{requestClient: receiver.rpc, failWithRetry: &remainingRetries}
	var attempts int
	got, err := runControlRequest(t.Context(), wrapped, protocolsession.MessageListChildren, []byte{0xa0}, func(call *sessioncore.Call) (protocolsession.OperationID, error) {
		attempts++
		if remainingRetries.Add(-1) >= 0 {
			return 0, sessioncore.ErrOperationRetry
		}
		return call.ID(), nil
	})
	if err != nil || attempts != 2 || got == 0 {
		t.Fatalf("recovered control request = (id=%d, attempts=%d, err=%v), want 2 attempts and nil err", got, attempts, err)
	}

	// Verify bounded retry cap prevents an infinite loop against a misbehaving sender.
	attempts = 0
	_, err = runControlRequest(t.Context(), receiver.rpc, protocolsession.MessageListChildren, []byte{0xa0}, func(*sessioncore.Call) (int, error) {
		attempts++
		return 0, sessioncore.ErrOperationRetry
	})
	if !errors.Is(err, sessioncore.ErrOperationRetry) || attempts != maxOperationWindowRetries+1 {
		t.Fatalf("capped control retry = (attempts=%d, err=%v), want (%d, ErrOperationRetry)", attempts, err, maxOperationWindowRetries+1)
	}

	// Verify RequestLane also recovers from ErrOperationRetry with a fresh OperationID.
	remainingRetries.Store(1)
	runtimeWithRetry := &Runtime{core: receiver.core, lifecycle: receiver.lifecycle, rpc: wrapped}
	grant, err := runtimeWithRetry.RequestLane(t.Context(), 0)
	if err != nil || grant.LaneID == 0 {
		t.Fatalf("RequestLane after replay-window retry = (%+v, %v)", grant, err)
	}
}

type retryInjectingRequestClient struct {
	requestClient
	failWithRetry *atomic.Int32
}

func (c *retryInjectingRequestClient) Await(ctx context.Context, call *sessioncore.Call) (protocolsession.Message, error) {
	if c.failWithRetry.Add(-1) >= 0 {
		return protocolsession.Message{}, sessioncore.ErrOperationRetry
	}
	return c.requestClient.Await(ctx, call)
}

func (c *retryInjectingRequestClient) AwaitCall(ctx context.Context, call *sessioncore.Call, cancelOnContext bool) (protocolsession.Message, error) {
	if c.failWithRetry.Add(-1) >= 0 {
		return protocolsession.Message{}, sessioncore.ErrOperationRetry
	}
	return c.requestClient.AwaitCall(ctx, call, cancelOnContext)
}

func (c *retryInjectingRequestClient) CancelAndEnd(call *sessioncore.Call, reason contentflow.CancelReason) error {
	return c.requestClient.CancelAndEnd(call, reason)
}

func (c *retryInjectingRequestClient) Runtime() *sessioncore.Runtime {
	return c.requestClient.Runtime()
}
