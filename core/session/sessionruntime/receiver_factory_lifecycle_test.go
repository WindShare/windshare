package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockingReceiverResourceLease struct {
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	releases atomic.Int32
}

func (lease *blockingReceiverResourceLease) Release() {
	lease.releases.Add(1)
	lease.once.Do(func() { close(lease.entered) })
	<-lease.release
}

func TestReceiverFactoryCloseCancelsAndJoinsConnectAdmissions(t *testing.T) {
	fixture := newVerticalFixture(t)
	lease := &blockingReceiverResourceLease{entered: make(chan struct{}), release: make(chan struct{})}
	var acquisitions atomic.Int32
	config := fixture.receiverConfig
	config.RuntimeResources = receiverResourceSourceFunc(func() (ReceiverRuntimeResourceLease, error) {
		acquisitions.Add(1)
		return lease, nil
	})
	factory, err := NewReceiverFactory(config)
	if err != nil {
		t.Fatal(err)
	}
	factory.mu.Lock()
	ownedAuthKey := factory.authKey
	factory.mu.Unlock()
	receiverChannel, peerChannel := newMemoryChannelPair()
	t.Cleanup(func() {
		_ = receiverChannel.Close()
		_ = peerChannel.Close()
	})
	connectResult := make(chan error, 1)
	go func() {
		_, connectErr := factory.Connect(context.Background(), receiverChannel)
		connectResult <- connectErr
	}()
	select {
	case <-peerChannel.Recv():
	case <-time.After(time.Second):
		t.Fatal("receiver did not reach the server-hello wait")
	}

	factory.BeginClose()
	select {
	case <-lease.entered:
	case <-time.After(time.Second):
		t.Fatal("factory close did not cancel the admitted handshake")
	}
	factory.mu.Lock()
	retainedDuringAdmission := len(factory.authKey) != 0
	factory.mu.Unlock()
	if !retainedDuringAdmission {
		t.Fatal("factory cleared its authentication key before admission cleanup returned")
	}
	select {
	case <-factory.closeDone:
		t.Fatal("factory published close before admission cleanup returned")
	default:
	}
	firstClose := make(chan struct{})
	secondClose := make(chan struct{})
	go func() { factory.Close(); close(firstClose) }()
	go func() { factory.Close(); close(secondClose) }()
	close(lease.release)
	if err := <-connectResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("factory-canceled Connect error=%v", err)
	}
	for index, completed := range []<-chan struct{}{firstClose, secondClose} {
		select {
		case <-completed:
		case <-time.After(time.Second):
			t.Fatalf("concurrent Close %d did not join", index)
		}
	}
	if acquisitions.Load() != 1 || lease.releases.Load() != 1 {
		t.Fatalf("resource lifecycle acquisitions=%d releases=%d", acquisitions.Load(), lease.releases.Load())
	}
	for index, value := range ownedAuthKey {
		if value != 0 {
			t.Fatalf("factory authentication key byte %d was not cleared", index)
		}
	}
	factory.mu.Lock()
	retainedAuthKey := factory.authKey
	factory.mu.Unlock()
	if retainedAuthKey != nil {
		t.Fatal("closed factory retained its authentication key slice")
	}
	if _, err := factory.Connect(context.Background(), newMemoryChannel(t)); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("Connect after Close error=%v", err)
	}
	if acquisitions.Load() != 1 {
		t.Fatalf("Connect after Close acquired resources; calls=%d", acquisitions.Load())
	}
}

func TestReceiverFactoryClosePreservesConnectedRuntime(t *testing.T) {
	fixture := newVerticalFixture(t)
	factory, err := NewReceiverFactory(fixture.receiverConfig)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, factory)
	defer sender.Close()
	defer receiver.Close()
	factory.Close()
	factory.mu.Lock()
	retainedDependencies := len(factory.publicKey) != 0 || !factory.descriptor.ShareInstance().IsZero() ||
		factory.verifier != nil || factory.opener != nil || factory.processReassembly != nil ||
		factory.shareReassembly != nil || factory.plaintextProcess != nil || factory.random != nil ||
		factory.admissionContext != nil || factory.cancelAdmissions != nil || factory.instances != nil ||
		factory.catalogProgress != nil || factory.semantic != nil || factory.resources != nil ||
		factory.now != nil || factory.after != nil
	factory.mu.Unlock()
	if retainedDependencies {
		t.Fatal("closed receiver factory retained its borrowed dependency graph")
	}
	if _, err := receiver.RequestLane(context.Background(), 0); err != nil {
		t.Fatalf("factory close damaged an already-owned runtime: %v", err)
	}
}

func TestReceiverFactoryAdmissionCallbackCanReenterBeginClose(t *testing.T) {
	fixture := newVerticalFixture(t)
	lease := &countedReceiverResourceLease{}
	var factory *ReceiverFactory
	config := fixture.receiverConfig
	config.RuntimeResources = receiverResourceSourceFunc(func() (ReceiverRuntimeResourceLease, error) {
		factory.BeginClose()
		return lease, nil
	})
	var err error
	factory, err = NewReceiverFactory(config)
	if err != nil {
		t.Fatal(err)
	}
	channel := newMemoryChannel(t)
	connectResult := make(chan error, 1)
	go func() {
		_, connectErr := factory.Connect(context.Background(), channel)
		connectResult <- connectErr
	}()
	select {
	case err := <-connectResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("reentrant close Connect error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("resource source reentrant BeginClose deadlocked Connect")
	}
	select {
	case <-factory.closeDone:
	case <-time.After(time.Second):
		t.Fatal("reentrant BeginClose did not publish completion")
	}
	if lease.releases.Load() != 1 {
		t.Fatalf("reentrant close resource releases=%d", lease.releases.Load())
	}
}
