package catalogflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func TestClientStopFromVerifierFreezesAdmissionAndCloseJoinsLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		instance := shareInstance(t, 181)
		directory := directoryID(t, 182)
		snapshot := onePageSnapshot(t, instance, directory, 183, "entry")
		page := snapshot.Pages()[0]

		verifierEntered := make(chan struct{})
		stopReturned := make(chan struct{})
		releaseVerifier := make(chan struct{})
		var transportCalls atomic.Int32
		transport := PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			transportCalls.Add(1)
			return []byte{1}, nil
		})
		var client *Client
		verifier := ObjectVerifierFunc(func(
			context.Context,
			catalog.ShareInstance,
			ListRequest,
			[]byte,
		) (VerifiedObject, error) {
			close(verifierEntered)
			client.Stop()
			close(stopReturned)
			<-releaseVerifier
			return VerifiedPage(page), nil
		})
		var err error
		client, err = NewClient(ClientConfig{
			ShareInstance: instance,
			Transport:     transport,
			Verifier:      verifier,
		})
		if err != nil {
			t.Fatal(err)
		}

		loadDone := make(chan error, 1)
		go func() {
			_, loadErr := client.LoadDirectory(context.Background(), directory)
			loadDone <- loadErr
		}()
		<-verifierEntered
		select {
		case <-stopReturned:
		case <-time.After(time.Second):
			t.Fatal("verifier callback deadlocked while stopping its client")
		}

		firstCloseDone := make(chan struct{})
		secondCloseDone := make(chan struct{})
		go func() {
			client.Close()
			close(firstCloseDone)
		}()
		go func() {
			client.Close()
			close(secondCloseDone)
		}()
		synctest.Wait()
		for name, done := range map[string]<-chan struct{}{
			"first":  firstCloseDone,
			"second": secondCloseDone,
		} {
			select {
			case <-done:
				t.Fatalf("%s Close returned while verifier still borrowed client resources", name)
			default:
			}
		}

		if _, loadErr := client.LoadDirectory(context.Background(), directory); !errors.Is(loadErr, ErrClientClosed) {
			t.Fatalf("load admitted after Stop: %v", loadErr)
		}
		_, release, acquireErr := client.AcquireDirectory(context.Background(), directory)
		release()
		if !errors.Is(acquireErr, ErrClientClosed) {
			t.Fatalf("acquire admitted after Stop: %v", acquireErr)
		}
		if transportCalls.Load() != 1 {
			t.Fatalf("closed client started %d transports, want 1", transportCalls.Load())
		}

		close(releaseVerifier)
		synctest.Wait()
		if loadErr := <-loadDone; !errors.Is(loadErr, ErrClientClosed) {
			t.Fatalf("stopped load error = %v", loadErr)
		}
		<-firstCloseDone
		<-secondCloseDone
		if _, committed := client.Snapshot(directory); committed {
			t.Fatal("load committed after Stop won the lifecycle race")
		}
		if client.CachedBytes() != 0 {
			t.Fatalf("closed client retained %d accounted bytes", client.CachedBytes())
		}
		client.Close()
		(*Client)(nil).Stop()
		(*Client)(nil).Close()
	})
}

func TestClientCloseDropsBorrowedGraphAndKeepsLateClaimReleaseSafe(t *testing.T) {
	instance := shareInstance(t, 184)
	directory := directoryID(t, 185)
	snapshot := onePageSnapshot(t, instance, directory, 186, "retained")
	page := snapshot.Pages()[0]
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			return []byte{1}, nil
		}),
		Verifier: ObjectVerifierFunc(func(
			context.Context,
			catalog.ShareInstance,
			ListRequest,
			[]byte,
		) (VerifiedObject, error) {
			return VerifiedPage(page), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, release, err := client.AcquireDirectory(context.Background(), directory)
	if err != nil || !loaded.Equal(snapshot) {
		t.Fatalf("acquire before close = %+v, %v", loaded, err)
	}

	client.Close()
	client.mu.Lock()
	retainedGraph := client.transport != nil || client.verifier != nil || client.now != nil ||
		client.cache != nil || client.inflight != nil || client.leaseClaimsByDirectory != nil
	cleaned := client.cleaned
	client.mu.Unlock()
	if !cleaned || retainedGraph {
		t.Fatalf("closed client retained borrowed/cache graph: cleaned=%v retained=%v", cleaned, retainedGraph)
	}
	if client.CachedBytes() != 0 {
		t.Fatalf("closed client retained %d accounted bytes", client.CachedBytes())
	}
	if _, ok := client.Snapshot(directory); ok {
		t.Fatal("closed client exposed a detached cache entry")
	}

	// The snapshot lease belongs to its caller, so closing the client must not
	// make the independently callable, idempotent release mutate reset counters.
	release()
	release()
	if client.CachedBytes() != 0 {
		t.Fatalf("late release changed closed accounting to %d", client.CachedBytes())
	}
}
