package receiversession

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/transfer"
)

func TestLostRenewalReauthorizesAtConfirmedExpiryWithoutLosingProgress(t *testing.T) {
	for _, direction := range []string{"request", "response"} {
		t.Run(direction, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newVerticalFixture(t)
				lease, err := content.NewRevisionLease(fixture.contentStore.lease.ID(), fixture.contentStore.descriptor,
					contentflow.RevisionLeaseTTL, contentflow.RevisionLeaseRenewAfter)
				if err != nil {
					t.Fatal(err)
				}
				fixture.contentStore.lease = lease
				ticks := make(chan time.Time, 1)
				config := fixture.receiverConfig
				config.After = func(time.Duration) <-chan time.Time { return ticks }
				factory, err := NewFactory(config)
				if err != nil {
					t.Fatal(err)
				}
				trackReceiverFactory(factory)
				sender, receiver, relaySender, relayReceiver := connectObservedVerticalPair(t, fixture.senderFactory, factory)
				defer sender.Close()
				defer receiver.Close()
				opened, err := receiver.access.OpenRevision(t.Context(), fixture.fileID)
				if err != nil {
					t.Fatal(err)
				}
				attachObservedLane(t, fixture.senderFactory, receiver, 0)
				receiver.revisions.mu.Lock()
				state := receiver.revisions.leases[lease.ID()]
				receiver.revisions.mu.Unlock()

				var loss atomic.Int32
				carrier := relaySender
				if direction == "request" {
					carrier = relayReceiver
				}
				carrier.gateMu.Lock()
				carrier.loseAndClose = &loss
				carrier.gateMu.Unlock()
				synctest.Wait()
				time.Sleep(contentflow.RevisionLeaseRenewAfter)
				ticks <- time.Now()
				synctest.Wait()
				if loss.Load() != 1 {
					t.Fatalf("renewal loss count = %d", loss.Load())
				}
				call := onlyActiveCall(t, receiver.rpc)
				_ = relaySender.Close()
				_ = relayReceiver.Close()
				synctest.Wait()
				if state.Err() != nil || onlyActiveCall(t, receiver.rpc) != call {
					t.Fatalf("carrier loss revoked valid authorization or replayed renewal: %v", state.Err())
				}

				// Read useful bytes while renewal evidence is missing. The same
				// revision handle must continue from that offset after reauthorization.
				output := make([]byte, len(fixture.fileData))
				split := uint64(len(output) / 2)
				sink := transfer.RangeSinkFunc(func(_ context.Context, offset uint64, data []byte) error {
					copy(output[offset:], data)
					return nil
				})
				dependencies := receiverTransferDependencies{runtime: receiver}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				err = dependencies.ReadRange(ctx, opened.Handle, opened.Descriptor, content.Range{Offset: 0, End: split}, sink)
				cancel()
				if err != nil {
					t.Fatalf("valid lease stopped reading after carrier loss: %v", err)
				}

				time.Sleep(contentflow.RevisionLeaseTTL - contentflow.RevisionLeaseRenewAfter)
				synctest.Wait()
				if !errors.Is(state.Err(), content.ErrLeaseExpired) {
					t.Fatalf("missing renewal did not expire at confirmed lifetime: %v", state.Err())
				}
				generation, authority := call.OperationAuthority()
				if !generation.IsZero() || !authority.IsZero() {
					t.Fatal("expired renewal retained operation authority")
				}
				next, err := content.NewRevisionLease(id16[content.LeaseID](10), fixture.contentStore.descriptor,
					contentflow.RevisionLeaseTTL, contentflow.RevisionLeaseRenewAfter)
				if err != nil {
					t.Fatal(err)
				}
				fixture.contentStore.lease = next
				ctx, cancel = context.WithTimeout(t.Context(), time.Second)
				err = dependencies.ReadRange(ctx, opened.Handle, opened.Descriptor,
					content.Range{Offset: split, End: uint64(len(output))}, sink)
				cancel()
				if err != nil {
					t.Fatalf("expired authorization did not recover through the same handle: %v", err)
				}
				if !bytes.Equal(output, fixture.fileData) {
					t.Fatal("lease replacement lost transfer progress")
				}
				expectedRenewals := int32(1)
				if direction == "request" {
					expectedRenewals = 0
				}
				if fixture.contentStore.renewCalls.Load() != expectedRenewals {
					t.Fatalf("non-repeatable renewal was replayed: calls=%d", fixture.contentStore.renewCalls.Load())
				}
				if receiver.Lifetime().Err() != nil || receiver.ProtocolSessionID() != sender.ProtocolSessionID() {
					t.Fatal("lease replacement lost the surviving protocol session")
				}
			})
		})
	}
}
