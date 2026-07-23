package v2endpoint

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/relay/signaling/v2route"
)

type counterConnectionIDSource struct {
	next    atomic.Uint64
	modulus uint64
}

func (source *counterConnectionIDSource) NewConnectionID() (v2route.ConnectionID, error) {
	sequence := source.next.Add(1) - 1
	return v2route.ConnectionID(fmt.Sprintf("counter-connection-%d", sequence%source.modulus)), nil
}

func newGenerationTestServer(
	t *testing.T,
	trace func(RetirementTrace),
) (*Server, *v2route.Registry, endpointFixture) {
	t.Helper()
	registry, err := v2route.New(context.Background(), v2route.Config{
		MaxRoutes: 2, MaxSessions: 8, MaxSessionsPerShare: 4,
		Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		registry: registry, connectionIDs: &counterConnectionIDSource{modulus: 1},
		connections: newConnectionRegistry(), retirementTracer: RetirementTraceFunc(trace),
	}
	return server, registry, newEndpointFixture(t)
}

func addGeneratedConnection(
	t *testing.T,
	server *Server,
	cancel context.CancelFunc,
) *connection {
	t.Helper()
	peer, err := server.newConnection(nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	if !server.connections.add(peer) {
		t.Fatalf("add generated connection %q generation %d", peer.ref.ConnectionID(), peer.ref.LocalGeneration())
	}
	return peer
}

func publishGenerationTestRoute(
	t *testing.T,
	registry *v2route.Registry,
	fixture endpointFixture,
	owner v2route.ConnectionRef,
) {
	t.Helper()
	if err := registry.BeginRegistration(fixture.init, owner); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(fixture.init.ShareID, owner, verifiedEndpointDescriptor(t, fixture)); err != nil {
		t.Fatal(err)
	}
}

func TestRouteRetirementDoesNotCancelReusedConnectionID(t *testing.T) {
	traces := make(chan RetirementTrace, 4)
	server, registry, fixture := newGenerationTestServer(t, func(event RetirementTrace) { traces <- event })
	var oldCancels atomic.Int32
	ownerA := addGeneratedConnection(t, server, func() { oldCancels.Add(1) })
	publishGenerationTestRoute(t, registry, fixture, ownerA.ref)
	retirement, transitioned := registry.UnexpectedDisconnect(fixture.init.ShareID, ownerA.ref)
	if !transitioned {
		t.Fatal("old generation did not produce a retirement")
	}
	if result := server.connections.detach(ownerA.ref); result != connectionExactCurrent {
		t.Fatalf("old generation detach = %s", result)
	}
	if !server.connections.complete(ownerA.ref) {
		t.Fatal("old generation did not complete")
	}

	var replacementCancels atomic.Int32
	ownerB := addGeneratedConnection(t, server, func() { replacementCancels.Add(1) })
	t.Cleanup(func() { server.connections.complete(ownerB.ref) })
	if ownerA.ref.ConnectionID() != ownerB.ref.ConnectionID() || ownerA.ref == ownerB.ref {
		t.Fatalf("counter source did not create an ABA pair: A=%+v B=%+v", ownerA.ref, ownerB.ref)
	}
	server.applyRouteRetirement(retirement, RetirementSourceDisconnect)
	if oldCancels.Load() != 0 || replacementCancels.Load() != 0 {
		t.Fatalf("delayed retirement cancels: old=%d replacement=%d", oldCancels.Load(), replacementCancels.Load())
	}
	if current, result, generation := server.connections.resolve(ownerB.ref); current != ownerB ||
		result != connectionExactCurrent || generation != ownerB.ref.LocalGeneration() {
		t.Fatalf("replacement lookup = %p, %s, %d", current, result, generation)
	}
	event := <-traces
	if event.ConnectionID != ownerA.ref.ConnectionID() || event.LocalGeneration != ownerA.ref.LocalGeneration() ||
		event.CurrentGeneration != ownerB.ref.LocalGeneration() || event.Source != RetirementSourceDisconnect ||
		event.Target != RetirementTargetOwner || event.CompareResult != RetirementCompareGenerationChanged || event.Applied {
		t.Fatalf("ABA trace = %+v", event)
	}
}

func TestCurrentConnectionGenerationRetiresExactly(t *testing.T) {
	traces := make(chan RetirementTrace, 2)
	server, registry, fixture := newGenerationTestServer(t, func(event RetirementTrace) { traces <- event })
	var cancels atomic.Int32
	owner := addGeneratedConnection(t, server, func() { cancels.Add(1) })
	t.Cleanup(func() { server.connections.complete(owner.ref) })
	publishGenerationTestRoute(t, registry, fixture, owner.ref)
	retirement, transitioned := registry.UnexpectedDisconnect(fixture.init.ShareID, owner.ref)
	if !transitioned {
		t.Fatal("current generation did not produce a retirement")
	}
	server.applyRouteRetirement(retirement, RetirementSourceDisconnect)
	if cancels.Load() != 1 {
		t.Fatalf("current generation cancel count = %d", cancels.Load())
	}
	event := <-traces
	if event.LocalGeneration != owner.ref.LocalGeneration() || event.CurrentGeneration != owner.ref.LocalGeneration() ||
		event.CompareResult != RetirementCompareExactCurrent || !event.Applied {
		t.Fatalf("current generation trace = %+v", event)
	}
}

func TestRepeatedRouteRetirementIsIdempotent(t *testing.T) {
	traces := make(chan RetirementTrace, 2)
	server, registry, fixture := newGenerationTestServer(t, func(event RetirementTrace) { traces <- event })
	var cancels atomic.Int32
	owner := addGeneratedConnection(t, server, func() { cancels.Add(1) })
	t.Cleanup(func() { server.connections.complete(owner.ref) })
	publishGenerationTestRoute(t, registry, fixture, owner.ref)
	retirement, transitioned := registry.UnexpectedDisconnect(fixture.init.ShareID, owner.ref)
	if !transitioned {
		t.Fatal("current generation did not produce a retirement")
	}
	server.applyRouteRetirement(retirement, RetirementSourceDisconnect)
	server.applyRouteRetirement(retirement, RetirementSourceDisconnect)
	if cancels.Load() != 1 {
		t.Fatalf("repeated retirement cancel count = %d", cancels.Load())
	}
	first, second := <-traces, <-traces
	if !first.Applied || second.Applied || first.CompareResult != RetirementCompareExactCurrent ||
		second.CompareResult != RetirementCompareExactCurrent {
		t.Fatalf("repeated retirement traces = %+v, %+v", first, second)
	}
}

func TestDelayedStopDetachAndReplacementStayGenerationBound(t *testing.T) {
	server, registry, fixture := newGenerationTestServer(t, nil)
	ownerA := addGeneratedConnection(t, server, func() {})
	publishGenerationTestRoute(t, registry, fixture, ownerA.ref)
	stop, authority := endpointStop(t, fixture)
	retirement, err := registry.Stop(context.Background(), stop, authority)
	if err != nil {
		t.Fatal(err)
	}
	if result := server.connections.detach(ownerA.ref); result != connectionExactCurrent {
		t.Fatalf("old detach = %s", result)
	}
	server.connections.complete(ownerA.ref)
	var replacementCancels atomic.Int32
	ownerB := addGeneratedConnection(t, server, func() { replacementCancels.Add(1) })
	t.Cleanup(func() { server.connections.complete(ownerB.ref) })

	start := make(chan struct{})
	detachResult := make(chan connectionCompareResult, 1)
	applyDone := make(chan struct{})
	go func() {
		<-start
		detachResult <- server.connections.detach(ownerA.ref)
	}()
	go func() {
		<-start
		server.applyRouteRetirement(retirement, RetirementSourceStop)
		close(applyDone)
	}()
	close(start)
	if result := <-detachResult; result != connectionGenerationChanged {
		t.Fatalf("delayed old detach compare = %s", result)
	}
	<-applyDone
	if replacementCancels.Load() != 0 {
		t.Fatalf("delayed STOP cancelled replacement %d time(s)", replacementCancels.Load())
	}
	if current, result, _ := server.connections.resolve(ownerB.ref); current != ownerB || result != connectionExactCurrent {
		t.Fatalf("replacement after delayed STOP = %p, %s", current, result)
	}
}

func TestConnectionRegistryCloseJoinsExactGenerations(t *testing.T) {
	server, _, _ := newGenerationTestServer(t, nil)
	oldCancelled := make(chan struct{}, 1)
	ownerA := addGeneratedConnection(t, server, func() { oldCancelled <- struct{}{} })
	if result := server.connections.detach(ownerA.ref); result != connectionExactCurrent {
		t.Fatalf("old detach = %s", result)
	}
	currentCancelled := make(chan struct{}, 1)
	ownerB := addGeneratedConnection(t, server, func() { currentCancelled <- struct{}{} })

	timedOut, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.connections.close(timedOut); err != context.Canceled {
		t.Fatalf("timed-out Close error = %v", err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- server.connections.close(context.Background()) }()
	for name, cancelled := range map[string]<-chan struct{}{
		"detached old generation": oldCancelled,
		"current replacement":     currentCancelled,
	} {
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatalf("Close did not cancel %s", name)
		}
	}
	if !server.connections.complete(ownerA.ref) {
		t.Fatal("old generation completion was lost")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before current generation completed: %v", err)
	default:
	}
	if !server.connections.complete(ownerB.ref) {
		t.Fatal("current generation completion was lost")
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	server.connections.mu.Lock()
	live, current := len(server.connections.live), len(server.connections.current)
	server.connections.mu.Unlock()
	if live != 0 || current != 0 {
		t.Fatalf("closed registry leaked generations: live=%d current=%d", live, current)
	}
	late, err := server.newConnection(nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if server.connections.add(late) {
		t.Fatal("closed registry admitted a new generation")
	}
}
