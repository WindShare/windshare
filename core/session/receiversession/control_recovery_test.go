package receiversession

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestControlTransactionsSettleConfirmedFramesLostWithCarrier(t *testing.T) {
	for _, operation := range []string{"catalog", "open", "renew", "release"} {
		for _, direction := range []string{"request", "response"} {
			t.Run(operation+"/"+direction, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					fixture := newVerticalFixture(t)
					sender, receiver, relaySender, relayReceiver := connectObservedVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
					defer sender.Close()
					defer receiver.Close()
					var lease content.LeaseID
					if operation == "renew" || operation == "release" {
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
						case "renew":
							_, err = receiver.revisions.renew(ctx, lease)
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
						if operation == "renew" {
							// A successful renewal changes its acceptance window, so an
							// ambiguous attempt must fail instead of being replayed.
							if !errors.Is(err, sessioncore.ErrRequestCarrierRetired) {
								t.Fatalf("ambiguous renewal was replayed: %v", err)
							}
						} else if err != nil {
							t.Fatalf("repeatable control did not recover: %v", err)
						}
					default:
						t.Fatal("lost response remained pending despite a surviving lane")
					}
					generation, authority := oldCall.OperationAuthority()
					if !generation.IsZero() || !authority.IsZero() {
						t.Fatal("retry retained the old operation's authority")
					}
					if operation != "renew" && (peerReceiver.sends.Load() <= peerSendsBefore || peerSender.sends.Load() <= 1) {
						t.Fatal("replacement lane carried no recovered transaction")
					}
					if receiver.Err() != nil || receiver.ProtocolSessionID() != sender.ProtocolSessionID() {
						t.Fatal("carrier recovery replaced or failed the protocol session")
					}
					requests.mu.Lock()
					issued := append([]protocolsession.OperationID(nil), requests.issued[issuedBefore:]...)
					requests.mu.Unlock()
					if operation == "renew" {
						if len(issued) != 1 || issued[0] != oldID {
							t.Fatalf("non-repeatable renewal issued another operation: %v", issued)
						}
					} else if len(issued) != 2 || issued[0] != oldID || issued[1] == oldID {
						t.Fatalf("recovery did not issue one fresh operation: %v", issued)
					}
				})
			})
		}
	}
}
