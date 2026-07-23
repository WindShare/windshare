package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/contentflow"
)

func TestDuplicateAuthenticatedLeaseIDFailClosesWithoutCompensatingSibling(t *testing.T) {
	fixture := newVerticalFixture(t)
	senderChannel, receiverChannel := newMemoryChannelPair()
	accepted := make(chan struct {
		runtime *SenderRuntime
		err     error
	}, 1)
	go func() {
		runtime, err := fixture.senderFactory.Accept(context.Background(), senderChannel)
		accepted <- struct {
			runtime *SenderRuntime
			err     error
		}{runtime: runtime, err: err}
	}()
	receiver, err := fixture.receiverFactory.Connect(context.Background(), receiverChannel)
	if err != nil {
		t.Fatal(err)
	}
	acceptedResult := <-accepted
	if acceptedResult.err != nil {
		receiver.Close()
		t.Fatal(acceptedResult.err)
	}
	sender := acceptedResult.runtime
	defer sender.Close()
	defer receiver.Close()

	var operationFrames atomic.Int32
	receiverChannel.pipe.mu.Lock()
	receiverChannel.onSend = func(framechannel.Frame) { operationFrames.Add(1) }
	receiverChannel.pipe.mu.Unlock()
	leaseID := fixture.contentStore.lease.ID()
	leaseContext, stopLease := context.WithCancel(receiver.revisions.ctx)
	existing := &remoteLeaseState{id: leaseID, ctx: leaseContext, cancel: stopLease}
	receiver.revisions.mu.Lock()
	receiver.revisions.leases[leaseID] = existing
	receiver.revisions.mu.Unlock()

	if _, err := receiver.OpenRevision(context.Background(), fixture.fileID); !errors.Is(err, ErrRemoteLeaseCollision) {
		t.Fatalf("duplicate lease error=%v", err)
	}
	if got := operationFrames.Load(); got != 1 {
		t.Fatalf("duplicate lease sent %d receiver frames; compensation would alias the sibling", got)
	}
	<-receiver.Done()
	if !errors.Is(receiver.Err(), ErrRemoteLeaseCollision) {
		t.Fatalf("duplicate lease terminal error=%v", receiver.Err())
	}
}

func TestStoppingLeaseCancelsInflightRenewBeforeRemoteHandlerReturns(t *testing.T) {
	fixture := newVerticalFixture(t)
	lease, err := content.NewRevisionLease(
		fixture.contentStore.lease.ID(), fixture.contentStore.descriptor,
		contentflow.RevisionLeaseTTL, contentflow.RevisionLeaseRenewAfter,
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.contentStore.lease = lease
	renewStarted := make(chan struct{}, 1)
	renewGate := make(chan struct{})
	var releaseGate sync.Once
	t.Cleanup(func() { releaseGate.Do(func() { close(renewGate) }) })
	fixture.contentStore.renewStart = renewStarted
	fixture.contentStore.renewGate = renewGate
	ticks := make(chan time.Time, 1)
	timerArmed := make(chan struct{}, 1)
	receiverConfig := fixture.receiverConfig
	receiverConfig.After = func(time.Duration) <-chan time.Time {
		timerArmed <- struct{}{}
		return ticks
	}
	receiverFactory, err := NewReceiverFactory(receiverConfig)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	opened, err := receiver.OpenRevision(context.Background(), fixture.fileID)
	if err != nil {
		t.Fatal(err)
	}
	<-timerArmed
	ticks <- time.Now()
	<-renewStarted

	receiver.revisions.mu.Lock()
	state := receiver.revisions.leases[opened.LeaseID]
	receiver.revisions.mu.Unlock()
	if state == nil {
		t.Fatal("opened lease has no local renewal state")
	}
	state.close()
	renewStopped := make(chan struct{})
	go func() {
		receiver.revisions.work.Wait()
		close(renewStopped)
	}()
	select {
	case <-renewStopped:
	case <-time.After(time.Second):
		t.Fatal("lease stop waited for a remote renew handler that ignored cancellation")
	}
	if !errors.Is(state.Err(), context.Canceled) {
		t.Fatalf("stopped in-flight renew error=%v", state.Err())
	}
	receiver.rpc.mu.Lock()
	activeCalls := len(receiver.rpc.calls)
	receiver.rpc.mu.Unlock()
	if activeCalls != 0 {
		t.Fatalf("stopped renew retained %d RPC calls", activeCalls)
	}

	releaseGate.Do(func() { close(renewGate) })
	if err := receiver.ReleaseRevision(context.Background(), opened.LeaseID); err != nil {
		t.Fatalf("release after canceled renew: %v", err)
	}
	if _, err := receiver.RequestLane(context.Background(), 0); err != nil {
		t.Fatalf("canceled renew damaged sibling operation: %v", err)
	}
}
