package receiversession

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/transfer"
)

func TestRenewalReplyMigratesAfterOriginalCarrierRetires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newVerticalFixture(t)
		lease, err := content.NewRevisionLease(fixture.contentStore.lease.ID(), fixture.contentStore.descriptor,
			contentflow.RevisionLeaseTTL, contentflow.RevisionLeaseRenewAfter)
		if err != nil {
			t.Fatal(err)
		}
		fixture.contentStore.lease = lease
		gate := make(chan struct{})
		release := sync.OnceFunc(func() { close(gate) })
		fixture.contentStore.renewGate = gate
		ticks := make(chan time.Time, 1)
		config := fixture.receiverConfig
		config.After = func(time.Duration) <-chan time.Time { return ticks }
		factory, err := NewFactory(config)
		if err != nil {
			t.Fatal(err)
		}
		trackReceiverFactory(factory)
		sender, receiver, relaySender, relayReceiver := connectObservedVerticalPair(t, fixture.senderFactory, factory)
		defer func() {
			release()
			receiver.Close()
			sender.Close()
		}()
		opened, err := receiver.access.OpenRevision(t.Context(), fixture.fileID)
		if err != nil {
			t.Fatal(err)
		}
		attachObservedLane(t, fixture.senderFactory, receiver, 0)
		ticks <- time.Now()
		synctest.Wait()
		if fixture.contentStore.renewCalls.Load() != 1 {
			t.Fatal("renewal did not reach sender")
		}
		_ = relaySender.Close()
		_ = relayReceiver.Close()
		synctest.Wait()
		if receiver.Lifetime().Err() != nil || !receiver.core.HasUsableLanes() {
			t.Fatal("fixture lost all paths")
		}
		release()
		synctest.Wait()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		dependencies := receiverTransferDependencies{runtime: receiver}
		err = dependencies.ReadRange(ctx, opened.Handle, opened.Descriptor,
			content.Range{Offset: 0, End: uint64(len(fixture.fileData))},
			transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil }))
		if err != nil {
			t.Fatalf("healthy surviving lane cannot continue after a migratable renewal reply: %v", err)
		}
		if fixture.contentStore.renewCalls.Load() != 1 {
			t.Fatal("renewal was replayed")
		}
	})
}
