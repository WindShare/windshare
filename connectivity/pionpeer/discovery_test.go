package pionpeer

import (
	"net"
	"net/netip"
	"strings"
	"testing"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

type gatedSTUN struct {
	net.PacketConn
	release <-chan struct{}
}

func (g gatedSTUN) WriteTo(b []byte, addr net.Addr) (int, error) {
	<-g.release
	return g.PacketConn.WriteTo(b, addr)
}

func TestTrickleDescriptionsPrecedeHostPortDiscovery(t *testing.T) {
	server, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	observed := make(chan netip.AddrPort, 4)
	go serveSTUN(gatedSTUN{server, release}, observed, nil)
	authority := socketauthority.New(socketauthority.Config{})
	defer authority.Close()
	var peers []*Connection
	var streams []chan *pion.ICECandidate
	for _, id := range []byte{1, 2} {
		lease := testLease(t, authority, id, "127.0.0.1")
		pc := testConnection(t, lease, []string{"stun:" + server.LocalAddr().String()}, nil)
		stream := make(chan *pion.ICECandidate, 16)
		pc.OnICECandidate(func(c *pion.ICECandidate) { stream <- c })
		peers = append(peers, pc)
		streams = append(streams, stream)
	}
	if _, err = peers[0].CreateDataChannel("trickle", nil); err != nil {
		t.Fatal(err)
	}
	for i, pc := range peers {
		var description pion.SessionDescription
		if i == 0 {
			description, err = pc.CreateOffer(nil)
		} else {
			description, err = pc.CreateAnswer(nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		gathered := pion.GatheringCompletePromise(pc.PeerConnection)
		if err = pc.SetLocalDescription(description); err != nil {
			t.Fatal(err)
		}
		await(t, gathered)
		description = *pc.LocalDescription()
		if strings.Contains(description.SDP, "a=end-of-candidates") {
			t.Fatal("SDP ended pending trickle gathering")
		}
		if err = peers[1-i].SetRemoteDescription(description); err != nil {
			t.Fatal(err)
		}
	}
	await(t, observed)
	close(release)
	for i, stream := range streams {
		seen := false
		for c := await(t, stream); c != nil; c = await(t, stream) {
			if c.Typ == pion.ICECandidateTypeSrflx {
				seen = true
				if err = peers[1-i].AddICECandidate(c.ToJSON()); err != nil {
					t.Fatal(err)
				}
			}
		}
		if !seen {
			t.Fatal("completion preceded mapped address")
		}
	}
}
