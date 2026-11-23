package pionpeer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	pion "github.com/pion/webrtc/v4"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

type pendingHostSTUN struct {
	started chan struct{}
	stopped chan error
}

func (p pendingHostSTUN) DiscoverXORMappedAddr(ctx context.Context, _ netip.AddrPort, _ *net.UDPAddr, _ time.Duration) (netip.AddrPort, error) {
	close(p.started)
	<-ctx.Done()
	p.stopped <- ctx.Err()
	return netip.AddrPort{}, ctx.Err()
}

func TestHostPortSTUNTimeoutCompletesGathering(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config, err := iceconfig.NewDirect([]string{"stun:127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		probe := pendingHostSTUN{make(chan struct{}), make(chan error, 1)}
		peer := &Connection{request: AttemptConfig{ICE: config}, mux: probe}
		dispatch := &candidateDispatch{ctx: t.Context(), wake: make(chan struct{}, 1)}
		events := make(chan *pion.ICECandidate, 4)
		go dispatch.run(peer, func(c *pion.ICECandidate) { events <- c })
		host := &pion.ICECandidate{Typ: pion.ICECandidateTypeHost, Protocol: pion.ICEProtocolUDP, Address: "127.0.0.1", Port: 2000}
		dispatch.post(candidateEvent{candidate: host})
		dispatch.post(candidateEvent{})
		<-probe.started
		if <-events != host {
			t.Fatal("STUN delayed the host candidate")
		}
		synctest.Wait()
		if len(events) != 0 {
			t.Fatal("gathering completed before the probe settled")
		}
		time.Sleep(hostSTUNTimeout)
		if err := <-probe.stopped; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if <-events != nil {
			t.Fatal("missing final gathering marker")
		}
	})
}

func TestClosingOrReplacingCandidateCallbackCancelsHostPortProbe(t *testing.T) {
	for _, action := range []string{"close", "replace"} {
		t.Run(action, func(t *testing.T) {
			authority := socketauthority.New(socketauthority.Config{})
			t.Cleanup(func() { _ = authority.Close() })
			lease := testLease(t, authority, 1, "127.0.0.1")
			peer := testConnection(t, lease, []string{"stun:127.0.0.1"}, nil)
			probe := pendingHostSTUN{make(chan struct{}), make(chan error, 1)}
			peer.mux = probe
			peer.OnICECandidate(nil)
			if _, err := peer.CreateDataChannel("candidate-owner", nil); err != nil {
				t.Fatal(err)
			}
			offer, err := peer.CreateOffer(nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = peer.SetLocalDescription(offer); err != nil {
				t.Fatal(err)
			}
			await(t, probe.started)
			if action == "close" {
				if err := peer.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				peer.OnICECandidate(func(*pion.ICECandidate) {})
			}
			if err := await(t, probe.stopped); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
