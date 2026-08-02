package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testrun"
)

const catalogGateTestDeadline = 5 * time.Second
const catalogGateNoEOFReadDeadline = 250 * time.Millisecond

func TestCatalogEnumerationGateRejectsForeignReleaseAndRetiresAfterOneBoundRelease(t *testing.T) {
	operation := catalogGateTestOperation(t)
	gate, err := newCatalogEnumerationGate(operation)
	if err != nil {
		t.Fatal(err)
	}
	admission := make(chan error, 1)
	go func() {
		admission <- gate.AdmitDirectoryScan(context.Background(), catalog.ScanRequest{})
	}()
	select {
	case <-gate.blocked:
	case <-time.After(catalogGateTestDeadline):
		t.Fatal("catalog scan did not reach the gate")
	}

	foreign := operation.EventIdentity()
	foreign.OperationID = "foreign-operation"
	if _, err := catalogGateExchange(gate.ListenerAddress(), catalogGateRequest(foreign, catalogGateActionRelease)); err == nil {
		t.Fatal("foreign release received a control response")
	}
	snapshot, err := catalogGateExchange(
		gate.ListenerAddress(),
		catalogGateRequest(operation.EventIdentity(), catalogGateActionSnapshot),
	)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Outcome != "observed" || snapshot.BlockedRequests != 1 || snapshot.Released {
		t.Fatalf("pre-release snapshot=%#v", snapshot)
	}
	released, err := catalogGateExchange(
		gate.ListenerAddress(),
		catalogGateRequest(operation.EventIdentity(), catalogGateActionRelease),
	)
	if err != nil {
		t.Fatal(err)
	}
	if released.Outcome != "released" || released.BlockedRequests != 1 || !released.Released {
		t.Fatalf("release response=%#v", released)
	}
	select {
	case err := <-admission:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(catalogGateTestDeadline):
		t.Fatal("released catalog scan remained blocked")
	}
	if _, err := catalogGateExchange(
		gate.ListenerAddress(),
		catalogGateRequest(operation.EventIdentity(), catalogGateActionRelease),
	); err == nil {
		t.Fatal("single-use release listener accepted a replay")
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogEnumerationGateCloseCancelsAndJoinsBlockedAdmission(t *testing.T) {
	gate, err := newCatalogEnumerationGate(catalogGateTestOperation(t))
	if err != nil {
		t.Fatal(err)
	}
	admission := make(chan error, 1)
	go func() {
		admission <- gate.AdmitDirectoryScan(context.Background(), catalog.ScanRequest{})
	}()
	select {
	case <-gate.blocked:
	case <-time.After(catalogGateTestDeadline):
		t.Fatal("catalog scan did not reach the gate")
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-admission:
		if !errors.Is(err, errCatalogGateClosed) {
			t.Fatalf("blocked admission error=%v", err)
		}
	case <-time.After(catalogGateTestDeadline):
		t.Fatal("gate close did not join blocked admission")
	}
}

func TestCatalogEnumerationGateRequiresEOFAndRejectsOversizedFrames(t *testing.T) {
	operation := catalogGateTestOperation(t)
	gate, err := newCatalogEnumerationGate(operation)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	admission := make(chan error, 1)
	go func() {
		admission <- gate.AdmitDirectoryScan(context.Background(), catalog.ScanRequest{})
	}()
	select {
	case <-gate.blocked:
	case <-time.After(catalogGateTestDeadline):
		t.Fatal("catalog scan did not reach the gate")
	}

	connection := catalogGateDial(t, gate.ListenerAddress())
	if err := ownerprotocol.WriteLineDocument(
		connection,
		catalogGateRequest(operation.EventIdentity(), catalogGateActionSnapshot),
	); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(catalogGateNoEOFReadDeadline)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := connection.Read(one[:]); err == nil {
		t.Fatal("gate responded before the request stream reached EOF")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("pre-EOF read error=%v, want deadline", err)
	}
	if err := connection.SetDeadline(time.Now().Add(catalogGateTestDeadline)); err != nil {
		t.Fatal(err)
	}
	if err := connection.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := ownerprotocol.ReadLineDocument[catalogGateControlResponse](connection)
	if err != nil || response.Outcome != "observed" || response.Released {
		t.Fatalf("EOF-framed snapshot=%#v err=%v", response, err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}

	oversized := catalogGateDial(t, gate.ListenerAddress())
	if _, err := oversized.Write(bytes.Repeat([]byte{'x'}, catalogGateMaximumBytes+1)); err != nil {
		t.Fatal(err)
	}
	if err := oversized.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerprotocol.ReadLineDocument[catalogGateControlResponse](oversized); err == nil {
		t.Fatal("oversized control frame received a response")
	}
	_ = oversized.Close()

	released, err := catalogGateExchange(
		gate.ListenerAddress(),
		catalogGateRequest(operation.EventIdentity(), catalogGateActionRelease),
	)
	if err != nil || !released.Released {
		t.Fatalf("release after framing adversaries=%#v err=%v", released, err)
	}
	select {
	case err := <-admission:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(catalogGateTestDeadline):
		t.Fatal("framing test release did not unblock admission")
	}
}

func TestCatalogEnumerationGateClosePromptlyJoinsStalledControlConnection(t *testing.T) {
	gate, err := newCatalogEnumerationGate(catalogGateTestOperation(t))
	if err != nil {
		t.Fatal(err)
	}
	connection := catalogGateDial(t, gate.ListenerAddress())
	defer connection.Close()
	select {
	case <-gate.accepted:
	case <-time.After(catalogGateTestDeadline):
		t.Fatal("gate did not publish its accepted connection")
	}
	closed := make(chan error, 1)
	go func() { closed <- gate.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("gate close waited for the stalled connection deadline")
	}
}

type acceptPauseCatalogGateListener struct {
	net.Listener
	accepted    chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func (listener *acceptPauseCatalogGateListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	close(listener.accepted)
	<-listener.release
	return connection, nil
}

func (listener *acceptPauseCatalogGateListener) Close() error {
	listener.releaseOnce.Do(func() { close(listener.release) })
	return listener.Listener.Close()
}

func TestCatalogEnumerationGateCloseJoinsAcceptBeforeActivePublication(t *testing.T) {
	underlying, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &acceptPauseCatalogGateListener{
		Listener: underlying,
		accepted: make(chan struct{}),
		release:  make(chan struct{}),
	}
	gate, err := newCatalogEnumerationGateWithListener(
		catalogGateTestOperation(t),
		func() (net.Listener, error) { return listener, nil },
	)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	connection := catalogGateDial(t, gate.ListenerAddress())
	defer connection.Close()
	select {
	case <-listener.accepted:
	case <-time.After(catalogGateTestDeadline):
		t.Fatal("listener did not accept the control connection")
	}

	closed := make(chan error, 1)
	go func() { closed <- gate.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("gate close did not join an accepted unpublished connection")
	}
}

func catalogGateTestOperation(t *testing.T) testrun.Operation {
	t.Helper()
	operation, err := testrun.NewOperation("catalog-gate-run", "catalog-gate-operation", "catalog-gate-scenario")
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func catalogGateRequest(identity testrun.Identity, action string) catalogGateControlRequest {
	return catalogGateControlRequest{
		SchemaVersion: catalogGateControlSchema,
		Identity:      identity,
		Action:        action,
	}
}

func catalogGateExchange(
	address string,
	request catalogGateControlRequest,
) (catalogGateControlResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), catalogGateTestDeadline)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return catalogGateControlResponse{}, err
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(catalogGateTestDeadline)); err != nil {
		return catalogGateControlResponse{}, err
	}
	if err := ownerprotocol.WriteLineDocument(connection, request); err != nil {
		return catalogGateControlResponse{}, err
	}
	if tcp, ok := connection.(*net.TCPConn); ok {
		if err := tcp.CloseWrite(); err != nil {
			return catalogGateControlResponse{}, err
		}
	}
	return ownerprotocol.ReadLineDocument[catalogGateControlResponse](connection)
}

func catalogGateDial(t *testing.T, address string) *net.TCPConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), catalogGateTestDeadline)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	tcp, ok := connection.(*net.TCPConn)
	if !ok {
		_ = connection.Close()
		t.Fatal("catalog gate did not use TCP")
	}
	if err := tcp.SetDeadline(time.Now().Add(catalogGateTestDeadline)); err != nil {
		_ = tcp.Close()
		t.Fatal(err)
	}
	return tcp
}
