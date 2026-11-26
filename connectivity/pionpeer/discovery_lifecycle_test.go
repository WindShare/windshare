package pionpeer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/iceconfig"
)

type lifecycleProbe func(context.Context, netip.AddrPort) (netip.AddrPort, error)

func (p lifecycleProbe) DiscoverXORMappedAddr(ctx context.Context, local netip.AddrPort, _ *net.UDPAddr, _ time.Duration) (netip.AddrPort, error) {
	return p(ctx, local)
}

// Pion stays idle. Driving its registered input directly allows synctest to
// place cancellation between an actual probe's final socket access and return.
func lifecyclePeer(t *testing.T, probe hostPortDiscovery) *Connection {
	t.Helper()
	pc, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	ice, err := iceconfig.NewDirect([]string{"stun:192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	c := &Connection{PeerConnection: pc, release: func() {}, mux: probe, request: AttemptConfig{ICE: ice}}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func lifecycleHost(port uint16) *pion.ICECandidate {
	return &pion.ICECandidate{Typ: pion.ICECandidateTypeHost, Protocol: pion.ICEProtocolUDP, Address: "192.0.2.2", Port: port, Component: 1}
}

func TestHostPortSTUNTimeoutCompletesGathering(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		stopped := make(chan error, 1)
		peer := lifecyclePeer(t, lifecycleProbe(func(ctx context.Context, _ netip.AddrPort) (netip.AddrPort, error) {
			close(started)
			<-ctx.Done()
			stopped <- ctx.Err()
			return netip.AddrPort{}, ctx.Err()
		}))
		events := make(chan *pion.ICECandidate, 4)
		peer.OnICECandidate(func(c *pion.ICECandidate) { events <- c })
		host := lifecycleHost(2000)
		peer.candidates.receive(host)
		peer.candidates.receive(nil)
		<-started
		if <-events != host {
			t.Fatal("STUN delayed the host candidate")
		}
		synctest.Wait()
		if len(events) != 0 {
			t.Fatal("gathering completed before the probe settled")
		}
		time.Sleep(hostSTUNTimeout)
		if err := <-stopped; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if <-events != nil {
			t.Fatal("missing final gathering marker")
		}
	})
}

func TestCandidateHandoffRetainsSocketUntilProbeReturns(t *testing.T) {
	for _, action := range []string{"close", "replace"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started, canceled, allowCleanup := make(chan struct{}), make(chan struct{}), make(chan struct{})
				successorStarted := make(chan struct{})
				oldHost, newHost := lifecycleHost(2000), lifecycleHost(2001)
				peer := lifecyclePeer(t, lifecycleProbe(func(ctx context.Context, local netip.AddrPort) (netip.AddrPort, error) {
					if local.Port() == oldHost.Port {
						close(started)
						<-ctx.Done()
						close(canceled)
						<-allowCleanup
					} else {
						close(successorStarted)
					}
					return netip.MustParseAddrPort("203.0.113.1:4000"), nil
				}))
				var observations []string
				peer.request.Observe = func(event Event) { observations = append(observations, event.Milestone) }
				released := false
				peer.release = func() { released = true }
				peer.OnICECandidate(nil)
				peer.candidates.receive(oldHost)
				<-started
				if action == "close" {
					peer.candidates.receive(nil)
				}
				finished := make(chan struct{})
				go func() {
					if action == "close" {
						_ = peer.Close()
					} else {
						peer.OnICECandidate(func(*pion.ICECandidate) {})
					}
					close(finished)
				}()
				<-canceled
				peer.candidates.receive(newHost)
				synctest.Wait()
				select {
				case <-finished:
					t.Fatal("handoff returned during old socket cleanup")
				case <-successorStarted:
					t.Fatal("successor used the socket during old cleanup")
				default:
				}
				if released {
					t.Fatal("lease released during old cleanup")
				}
				close(allowCleanup)
				<-finished
				if action == "replace" {
					<-successorStarted
					peer.candidates.receive(nil)
				}
				synctest.Wait()
				want := []string{"candidate", "provider_closed"}
				if action == "replace" {
					want = []string{"candidate", "candidate", HostSTUNDiscovered, "candidate", GatheringComplete}
				}
				if !slices.Equal(observations, want) {
					t.Fatalf("observations = %v, want %v", observations, want)
				}
			})
		})
	}
}

func TestCandidateCallbackReplacesItselfWithoutOverlappingSuccessor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		peer := lifecyclePeer(t, nil)
		entered, replace, replaced, leave := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var received []*pion.ICECandidate
		peer.OnICECandidate(func(*pion.ICECandidate) {
			close(entered)
			<-replace
			peer.OnICECandidate(func(c *pion.ICECandidate) { received = append(received, c) })
			close(replaced)
			<-leave
		})
		peer.candidates.receive(lifecycleHost(2000))
		<-entered
		peer.candidates.receive(lifecycleHost(2001))
		close(replace)
		<-replaced
		valid := lifecycleHost(2002)
		peer.candidates.receive(valid)
		peer.candidates.receive(nil)
		synctest.Wait()
		if len(received) != 0 {
			t.Fatal("new callback ran inside its predecessor")
		}
		close(leave)
		synctest.Wait()
		if !slices.Equal(received, []*pion.ICECandidate{valid, nil}) {
			t.Fatalf("replacement received stale or unordered candidates: %+v", received)
		}
	})
}

func TestCandidateCloseIsReentrantAndRevokesQueuedCompletion(t *testing.T) {
	for _, caller := range []string{"callback", "observer"} {
		t.Run(caller, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peer := lifecyclePeer(t, nil)
				entered, queued, closed, leave := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
				closeInside := func() {
					close(entered)
					<-queued
					_ = peer.Close()
					close(closed)
					<-leave
				}
				var observations []string
				peer.request.Observe = func(e Event) {
					observations = append(observations, e.Milestone)
					if caller == "observer" && e.Candidate != nil {
						closeInside()
					}
				}
				var callbacks int
				peer.OnICECandidate(func(*pion.ICECandidate) {
					callbacks++
					if caller == "callback" {
						closeInside()
					}
				})
				peer.candidates.receive(lifecycleHost(2000))
				<-entered
				peer.candidates.receive(nil)
				close(queued)
				<-closed
				peer.candidates.receive(lifecycleHost(2001))
				close(leave)
				synctest.Wait()
				wantCallbacks := 0
				if caller == "callback" {
					wantCallbacks = 1
				}
				if callbacks != wantCallbacks || !slices.Equal(observations, []string{"candidate", "provider_closed"}) {
					t.Fatalf("callbacks = %d, observations = %v", callbacks, observations)
				}
			})
		})
	}
}

func TestCloseRunsOnClosedSynchronouslyBeforeDeferredOrConcurrentObservation(t *testing.T) {
	t.Run("active observation deferral", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			peer := lifecyclePeer(t, nil)
			entered, releaseObserver := make(chan struct{}), make(chan struct{})
			var observations []string
			peer.request.Observe = func(e Event) {
				observations = append(observations, e.Milestone)
				if e.Candidate != nil {
					close(entered)
					<-releaseObserver
				}
			}
			leaseReleased := false
			peer.release = func() { leaseReleased = true }
			onClosedCalls := 0
			sawLeaseReleased := false
			peer.request.OnClosed = func() {
				onClosedCalls++
				sawLeaseReleased = leaseReleased
			}
			peer.OnICECandidate(func(*pion.ICECandidate) {})
			peer.candidates.receive(lifecycleHost(2000))
			<-entered

			if err := peer.Close(); err != nil {
				t.Fatalf("Close err = %v", err)
			}
			if onClosedCalls != 1 || !sawLeaseReleased {
				t.Fatalf("OnClosed calls = %d, sawLeaseReleased = %v", onClosedCalls, sawLeaseReleased)
			}
			if !slices.Equal(observations, []string{"candidate"}) {
				t.Fatalf("observations before observer exit = %v, want [candidate]", observations)
			}

			close(releaseObserver)
			synctest.Wait()
			if onClosedCalls != 1 || !slices.Equal(observations, []string{"candidate", ProviderClosed}) {
				t.Fatalf("final OnClosed calls = %d, observations = %v", onClosedCalls, observations)
			}
		})
	})

	t.Run("concurrent close", func(t *testing.T) {
		peer := lifecyclePeer(t, nil)
		onClosedEntered, releaseOnClosed := make(chan struct{}), make(chan struct{})
		providerClosedEntered, releaseProviderClosed := make(chan struct{}), make(chan struct{})
		leaseReleased := false
		peer.release = func() { leaseReleased = true }
		onClosedDone := false
		onClosedCalls := 0
		peer.request.OnClosed = func() {
			onClosedCalls++
			if !leaseReleased {
				t.Error("OnClosed ran before socket release")
			}
			close(onClosedEntered)
			<-releaseOnClosed
			onClosedDone = true
		}
		var observations []string
		peer.request.Observe = func(e Event) {
			if e.Milestone == ProviderClosed {
				close(providerClosedEntered)
				<-releaseProviderClosed
			}
			observations = append(observations, e.Milestone)
		}

		firstDone := make(chan error, 1)
		go func() { firstDone <- peer.Close() }()
		<-onClosedEntered

		secondStarted := make(chan struct{})
		secondDone := make(chan error, 1)
		go func() {
			close(secondStarted)
			secondDone <- peer.Close()
		}()
		<-secondStarted
		select {
		case err := <-secondDone:
			t.Fatalf("concurrent Close returned before OnClosed finished: %v", err)
		default:
		}

		close(releaseOnClosed)
		<-providerClosedEntered
		if err := <-secondDone; err != nil {
			t.Fatalf("concurrent Close err = %v", err)
		}
		if !onClosedDone || onClosedCalls != 1 {
			t.Fatalf("after concurrent Close: onClosedDone = %v, calls = %d", onClosedDone, onClosedCalls)
		}

		close(releaseProviderClosed)
		if err := <-firstDone; err != nil {
			t.Fatalf("winner Close err = %v", err)
		}
		if onClosedCalls != 1 || !slices.Equal(observations, []string{ProviderClosed}) {
			t.Fatalf("final OnClosed calls = %d, observations = %v", onClosedCalls, observations)
		}
	})
}

func TestLateActiveTCPCandidateObservedAfterEndOfTrickle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		peer := lifecyclePeer(t, nil)
		var observations []Event
		peer.request.Observe = func(e Event) { observations = append(observations, e) }
		var callbacks []*pion.ICECandidate
		callback := func(c *pion.ICECandidate) { callbacks = append(callbacks, c) }
		peer.OnICECandidate(callback)
		host := lifecycleHost(2000)
		peer.candidates.receive(host)
		peer.candidates.receive(nil)
		synctest.Wait()
		if peer.candidates.current.callback != nil || peer.candidates.current.seen != nil {
			t.Fatal("completed gathering retained its signaling callback or mapped candidates")
		}
		active := &pion.ICECandidate{Typ: pion.ICECandidateTypeHost, Protocol: pion.ICEProtocolTCP, Address: "192.0.2.2", Port: 2001, Component: 1, TCPType: "active"}
		peer.OnICECandidate(callback)
		peer.candidates.receive(active)
		peer.candidates.receive(nil)
		synctest.Wait()
		if !slices.Equal(callbacks, []*pion.ICECandidate{host, nil}) {
			t.Fatalf("candidate escaped the terminal marker: %+v", callbacks)
		}
		if len(observations) != 3 || observations[1].Milestone != GatheringComplete || observations[2].Candidate == nil || observations[2].Candidate.TCPType != "active" {
			t.Fatalf("late TCP observation or unique completion missing: %+v", observations)
		}
		if peer.candidates.draining || peer.candidates.queue != nil || peer.candidates.current.callback != nil {
			t.Fatal("idle candidate stream retained work")
		}
	})
}
