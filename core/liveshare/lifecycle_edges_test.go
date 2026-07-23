package liveshare

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestSenderCatalogAccessRejectsClosedAdmissionAndStopsIdempotently(t *testing.T) {
	var absent *senderCatalogAccess
	if _, err := absent.NewSenderCatalogService(); err == nil {
		t.Fatal("nil catalog access admitted a sender service")
	}
	absent.StartRootPrefetch()
	absent.CancelRootPrefetch()
	absent.Close()

	closed := &senderCatalogAccess{closed: true, wake: make(chan struct{}, 1)}
	if _, err := closed.NewSenderCatalogService(); err == nil {
		t.Fatal("closed catalog access admitted a sender service")
	}
	closed.StartRootPrefetch()
	closed.Close()

	lifetime, cancelLifetime := context.WithCancel(context.Background())
	active, cancelActive := context.WithCancel(context.Background())
	access := &senderCatalogAccess{
		wake:            make(chan struct{}, 1),
		lifetimeCancel:  cancelLifetime,
		activeCancel:    cancelActive,
		prefetchStarted: true,
	}
	access.CancelRootPrefetch()
	access.CancelRootPrefetch()
	for name, done := range map[string]<-chan struct{}{
		"lifetime": lifetime.Done(),
		"active":   active.Done(),
	} {
		select {
		case <-done:
		default:
			t.Fatalf("%s prefetch scope was not canceled", name)
		}
	}
	access.Close()
	access.Close()
}

func TestSenderCatalogAccessPrefetchAdmissionObservesLifetimeAndStopAuthority(t *testing.T) {
	access := &senderCatalogAccess{wake: make(chan struct{}, 1)}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := access.beginPrefetchAttempt(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled prefetch lifetime = %v", err)
	}

	access.prefetchStopped = true
	if _, _, err := access.beginPrefetchAttempt(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped prefetch admission = %v", err)
	}

	access.prefetchStopped = false
	attemptContext, attempt, err := access.beginPrefetchAttempt(context.Background())
	if err != nil || attempt == 0 || attemptContext.Err() != nil {
		t.Fatalf("active prefetch attempt = %d, %v", attempt, err)
	}
	access.finishPrefetchAttempt(attempt)
	if attemptContext.Err() == nil || access.activeAttempt != 0 || access.activeCancel != nil {
		t.Fatal("finished prefetch retained active authority")
	}
}

func TestFacadeAndRuntimeResourceShutdownAreNilSafeAndIdempotent(t *testing.T) {
	var sender *PreparedSender
	sender.StartRootPrefetch()
	if err := sender.Stop(); err != nil {
		t.Fatalf("nil sender stop = %v", err)
	}

	closedSender := &PreparedSender{closed: true}
	closedSender.StartRootPrefetch()
	if err := closedSender.Stop(); err != nil {
		t.Fatalf("closed sender stop = %v", err)
	}

	var receiver *PreparedReceiver
	receiver.BeginClose()
	receiver.WaitClosed()
	receiver.Close()

	var absentResources *receiverRuntimeResources
	absentResources.Close()
	var absentLease *receiverRuntimeResourceLease
	absentLease.Release()

	resources := newReceiverRuntimeResources(nil, nil)
	lease, err := resources.AcquireReceiverRuntimeResources()
	if err != nil {
		t.Fatal(err)
	}
	resources.Close()
	lease.Release()
	lease.Release()
	resources.mu.Lock()
	active, closed := resources.active, resources.closed
	resources.mu.Unlock()
	if active != 0 || !closed {
		t.Fatalf("released runtime resources active=%d closed=%v", active, closed)
	}
	if _, err := resources.AcquireReceiverRuntimeResources(); !errors.Is(err, errReceiverClosed) {
		t.Fatalf("closed runtime resource admission = %v", err)
	}
}

func TestRandomIdentityRejectsZeroEntropyWithoutWeakeningIdentityType(t *testing.T) {
	identity, err := randomIdentity(bytes.NewReader(make([]byte, catalog.IdentityBytes)))
	if err == nil || identity != ([catalog.IdentityBytes]byte{}) {
		t.Fatalf("zero random identity = %x, %v", identity, err)
	}
}
