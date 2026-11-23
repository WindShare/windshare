package pionpeer

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"testing/synctest"

	pion "github.com/pion/webrtc/v4"
)

func TestCandidateHandoffDeliversPendingEndAfterProbeCleanup(t *testing.T) {
	for _, end := range []string{"before handoff", "during cleanup"} {
		t.Run(end, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				peer := lifecyclePeer(t, lifecycleProbe(func(ctx context.Context, _ netip.AddrPort) (netip.AddrPort, error) {
					close(started)
					<-ctx.Done()
					close(canceled)
					<-release
					return netip.AddrPort{}, ctx.Err()
				}))
				var observed []string
				peer.request.Observe = func(e Event) { observed = append(observed, e.Milestone) }
				var old, next []*pion.ICECandidate
				peer.OnICECandidate(func(c *pion.ICECandidate) { old = append(old, c) })
				host := lifecycleHost(2000)
				peer.candidates.receive(host)
				<-started
				if end == "before handoff" {
					peer.candidates.receive(nil)
				}
				synctest.Wait()
				go peer.OnICECandidate(func(c *pion.ICECandidate) { next = append(next, c) })
				<-canceled
				if end == "during cleanup" {
					peer.candidates.receive(nil)
				}
				synctest.Wait()
				if len(next) != 0 {
					t.Fatal("end delivered before canceled probe cleanup")
				}
				close(release)
				synctest.Wait()
				if !slices.Equal(old, []*pion.ICECandidate{host}) || !slices.Equal(next, []*pion.ICECandidate{nil}) {
					t.Fatalf("old = %v, next = %v", old, next)
				}
				if !slices.Equal(observed, []string{"candidate"}) {
					t.Fatalf("canceled discovery reported a gathering outcome: %v", observed)
				}
			})
		})
	}
}

func TestCandidateHandoffDropsQueuedHostWithoutFailingProfile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var probes, gatherings int
		peer := lifecyclePeer(t, lifecycleProbe(func(context.Context, netip.AddrPort) (netip.AddrPort, error) {
			probes++
			return netip.MustParseAddrPort("203.0.113.1:4000"), nil
		}))
		peer.request.Observe = func(e Event) {
			if e.Milestone == GatheringComplete {
				gatherings++
			}
		}
		entered, leave := make(chan struct{}), make(chan struct{})
		peer.OnICECandidate(func(*pion.ICECandidate) {
			close(entered)
			<-leave
		})
		first := lifecycleHost(2000)
		first.Protocol = pion.ICEProtocolTCP
		peer.candidates.receive(first)
		<-entered
		peer.candidates.receive(lifecycleHost(2001))
		var next []*pion.ICECandidate
		peer.OnICECandidate(func(c *pion.ICECandidate) { next = append(next, c) })
		peer.candidates.receive(nil)
		close(leave)
		synctest.Wait()
		if probes != 0 || gatherings != 0 || !slices.Equal(next, []*pion.ICECandidate{nil}) {
			t.Fatalf("probes = %d, gathering observations = %d, successor events = %v", probes, gatherings, next)
		}
	})
}

func TestReentrantHandoffCommitsTrickleEndOnce(t *testing.T) {
	for _, replaceAt := range []string{"last mapped candidate", "gathering observer", "terminal callback"} {
		t.Run(replaceAt, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				peer := lifecyclePeer(t, lifecycleProbe(func(context.Context, netip.AddrPort) (netip.AddrPort, error) {
					<-release
					return netip.MustParseAddrPort("203.0.113.1:4000"), nil
				}))
				var next []*pion.ICECandidate
				replace := func() { peer.OnICECandidate(func(c *pion.ICECandidate) { next = append(next, c) }) }
				var gathering, successes, mapped, oldEnds int
				peer.request.Observe = func(e Event) {
					switch e.Milestone {
					case HostSTUNDiscovered:
						successes++
					case GatheringComplete:
						gathering++
						if replaceAt == "gathering observer" {
							replace()
						}
					}
				}
				peer.OnICECandidate(func(c *pion.ICECandidate) {
					if c == nil {
						oldEnds++
						if replaceAt == "terminal callback" {
							replace()
						}
					} else if c.Typ == pion.ICECandidateTypeSrflx {
						mapped++
						if replaceAt == "last mapped candidate" {
							replace()
						}
					}
				})
				peer.candidates.receive(lifecycleHost(2000))
				peer.candidates.receive(nil)
				synctest.Wait()
				close(release)
				synctest.Wait()
				if successes != 1 || mapped != 1 || gathering > 1 {
					t.Fatalf("STUN successes = %d, mapped candidates = %d, gathering observations = %d", successes, mapped, gathering)
				}
				if replaceAt == "terminal callback" {
					if oldEnds != 1 || len(next) != 0 {
						t.Fatalf("committed end replayed: old = %d, next = %v", oldEnds, next)
					}
				} else if oldEnds != 0 || !slices.Equal(next, []*pion.ICECandidate{nil}) {
					t.Fatalf("pending end handoff: old = %d, next = %v", oldEnds, next)
				}
			})
		})
	}
}
