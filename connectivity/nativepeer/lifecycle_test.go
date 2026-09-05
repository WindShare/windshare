package nativepeer

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

type mappedGateway struct{ deletes atomic.Int32 }

func (g *mappedGateway) Create(_ context.Context, request reachability.Request) (reachability.Lease, error) {
	return reachability.Lease{External: netip.MustParseAddrPort("8.8.8.8:4242"), Lifetime: time.Minute, GatewayID: "fake", ResourceID: "mapping", Kind: "pcp"}, nil
}
func (g *mappedGateway) Renew(ctx context.Context, request reachability.Request, _ reachability.Lease) (reachability.Lease, error) {
	return g.Create(ctx, request)
}
func (g *mappedGateway) Delete(context.Context, reachability.Request, reachability.Lease) error {
	g.deletes.Add(1)
	return nil
}
func TestMappingFactsAreFrozenOnlyIntoFreshAttemptAndRetainedOnlyForSelectedLane(t *testing.T) {
	h := newHarness(t)
	gateway := &mappedGateway{}
	h.native.config.Reachability = reachability.New(reachability.Config{Now: func() time.Time { return h.now }, Gateways: []reachability.Gateway{gateway}})
	req := request(1)
	changes, _ := h.native.SetDemand(req.ProtocolSessionID, req.Binding.PeerPathID, true, false)
	_, _ = h.native.NewPeerConnection(context.Background(), req)
	if len(h.requests[0].MappedEndpoints) != 0 {
		t.Fatal("mapping appeared before gateway response")
	}
	h.native.Maintain(context.Background())
	h.now = h.now.Add(3 * time.Second)
	h.native.Maintain(context.Background())
	select {
	case change := <-changes:
		if !change.MappingReady {
			t.Fatal(change)
		}
	default:
		t.Fatal("missing mapping readiness")
	}
	if len(h.requests[0].MappedEndpoints) != 0 {
		t.Fatal("late mapping mutated active snapshot")
	}
	_, _ = h.native.NewPeerConnection(context.Background(), request(2))
	second := h.requests[1]
	if len(second.MappedEndpoints) != 1 || second.MappedEndpoints[0].Local != second.SocketLease.Endpoints()[0] {
		t.Fatal(second.MappedEndpoints)
	}
	second.Observe(provider.Event{Pair: &provider.PairFacts{LocalAddress: "8.8.8.8", LocalPort: 4242, Protocol: "udp"}})
	h.native.SetDirect(req.ProtocolSessionID, req.Binding.PeerPathID)
	if len(h.native.config.Reachability.Facts()) != 1 {
		t.Fatal("admitted mapping lost its lease")
	}
	if err := h.native.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gateway.deletes.Load() != 1 {
		t.Fatal("final demand did not revoke exact lease", gateway.deletes.Load())
	}
	if _, err := h.native.NewPeerConnection(context.Background(), request(3)); err == nil {
		t.Fatal("closed owner reopened")
	}
	if err := h.native.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestSenderMaintenancePublishesOnlyNewLocalFacts(t *testing.T) {
	h := newHarness(t)
	session := [16]byte{1}
	pathID := v2signal.PeerPathID{2}
	h.native.maintainSession(context.Background(), session, nil)
	changes, _ := h.native.SetDemand(session, pathID, true, false)
	_ = changes
	path := h.native.paths[pathKey{session, pathID}]
	notify(path, Change{MappingReady: true})
	var sent []protocolsession.PeerPathControl
	send := func(_ context.Context, body []byte) error {
		control, err := protocolsession.DecodePeerPathControl(body)
		if err == nil {
			sent = append(sent, control)
		}
		return err
	}
	h.native.maintainSession(context.Background(), session, send)
	if len(sent) != 1 || sent[0].Kind != protocolsession.PeerPathMappingReady {
		t.Fatal(sent)
	}
	notify(path, Change{Remote: true, MappingReady: true})
	h.native.maintainSession(context.Background(), session, send)
	if len(sent) != 1 {
		t.Fatal("remote mapping echoed")
	}
	notify(path, Change{NetworkGenerationID: 2})
	h.native.maintainSession(context.Background(), session, send)
	if len(sent) != 2 || sent[1].Kind != protocolsession.PeerPathNetworkChanged {
		t.Fatal(sent)
	}
	notify(path, Change{MappingReady: true})
	notify(path, Change{NetworkGenerationID: 4})
	h.native.maintainSession(context.Background(), session, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.native.RunSession(ctx, session, send)
	h.native.SetDirect(session, v2signal.PeerPathID{255})
	if h.native.Retired(session, pathID) {
		t.Fatal("live path retired")
	}
	if _, ok := h.native.ApplyControl(session, controlBytes(t, 1, protocolsession.PeerPathRevoke, 0)); !ok {
		t.Fatal("revoke")
	}
	if !h.native.Retired(session, pathID) {
		t.Fatal("retirement lost")
	}
	if _, ok := h.native.ApplyControl(session, controlBytes(t, 2, protocolsession.PeerPathDemand, ControlLifetime)); ok {
		t.Fatal("revoked path resurrected")
	}
}
func TestIdleUsesOnlyBoundedPreviouslySelectedProfileAndOwnerCloseJoinsGenerationCleanup(t *testing.T) {
	h := newHarness(t)
	h.native.Idle(context.Background(), [16]byte{1}, v2signal.PeerPathID{2}, h.now)
	_, _ = h.native.NewPeerConnection(context.Background(), request(1))
	path := h.native.paths[pathKey{[16]byte{1}, v2signal.PeerPathID{2}}]
	path.lastURLs = []string{"bad-url", "stun:127.0.0.1:zero", "stun:127.0.0.1:0", "stun:127.0.0.1:1"}
	h.native.Idle(context.Background(), [16]byte{1}, v2signal.PeerPathID{2}, h.now)
	if err := h.native.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	automatic := New(Config{Monitor: h.monitor, Now: func() time.Time { return h.now }, Pool: h.native.config.Pool, Connect: func(pion.Configuration, provider.AttemptConfig) (*provider.Connection, error) {
		return nil, context.Canceled
	}})
	_, _ = automatic.NewPeerConnection(context.Background(), request(1))
	h.monitor.state.ResumeSequence++
	_, _ = automatic.NewPeerConnection(context.Background(), request(2))
	if err := automatic.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	defaults := New(Config{})
	if err := defaults.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
