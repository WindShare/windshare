package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testrun"
)

const (
	catalogGateControlSchema      = "windshare.catalog-enumeration-gate-control/v1"
	catalogGateResponseSchema     = "windshare.catalog-enumeration-gate-response/v1"
	catalogGateActionSnapshot     = "snapshot"
	catalogGateActionRelease      = "release"
	catalogGateMaximumBytes       = 1024
	catalogGateMaximumAttempts    = 4
	catalogGateConnectionDeadline = 5 * time.Second
)

var errCatalogGateClosed = errors.New("catalog enumeration gate closed before release")

type catalogGateControlRequest struct {
	SchemaVersion string `json:"schema_version"`
	testrun.Identity
	Action string `json:"action"`
}

type catalogGateControlResponse struct {
	SchemaVersion   string `json:"schema_version"`
	Action          string `json:"action"`
	Outcome         string `json:"outcome"`
	BlockedRequests uint64 `json:"blocked_requests"`
	Released        bool   `json:"released"`
}

type catalogEnumerationGate struct {
	listener net.Listener
	address  string
	identity testrun.Identity
	settled  chan struct{}
	blocked  chan struct{}
	accepted chan struct{}
	done     chan struct{}

	mu              sync.Mutex
	active          net.Conn
	blockedRequests uint64
	released        bool
	closed          bool
	serveErr        error
	blockedOnce     sync.Once
	acceptedOnce    sync.Once
	closeOnce       sync.Once
	closeErr        error
}

func newCatalogEnumerationGate(operation testrun.Operation) (*catalogEnumerationGate, error) {
	return newCatalogEnumerationGateWithListener(operation, func() (net.Listener, error) {
		return net.Listen("tcp", "127.0.0.1:0")
	})
}

func newCatalogEnumerationGateWithListener(
	operation testrun.Operation,
	openListener func() (net.Listener, error),
) (*catalogEnumerationGate, error) {
	identity := operation.EventIdentity()
	if err := testrun.ValidateIdentity(identity); err != nil {
		return nil, fmt.Errorf("create catalog enumeration gate: invalid operation: %w", err)
	}
	if openListener == nil {
		return nil, errors.New("create catalog enumeration gate: listener opener is nil")
	}
	listener, err := openListener()
	if err != nil {
		return nil, fmt.Errorf("create catalog enumeration gate listener: %w", err)
	}
	if listener == nil {
		return nil, errors.New("create catalog enumeration gate: listener opener returned nil")
	}
	gate := &catalogEnumerationGate{
		listener: listener,
		address:  listener.Addr().String(),
		identity: identity,
		settled:  make(chan struct{}),
		blocked:  make(chan struct{}),
		accepted: make(chan struct{}),
		done:     make(chan struct{}),
	}
	go gate.serve()
	return gate, nil
}

func (gate *catalogEnumerationGate) ListenerAddress() string {
	if gate == nil {
		return ""
	}
	return gate.address
}

func (gate *catalogEnumerationGate) AdmitDirectoryScan(
	ctx context.Context,
	_ catalog.ScanRequest,
) error {
	if gate == nil {
		return errors.New("catalog enumeration gate is nil")
	}
	gate.mu.Lock()
	if gate.released {
		gate.mu.Unlock()
		return nil
	}
	if gate.closed {
		gate.mu.Unlock()
		return errCatalogGateClosed
	}
	gate.blockedRequests++
	settled := gate.settled
	gate.blockedOnce.Do(func() { close(gate.blocked) })
	gate.mu.Unlock()

	select {
	case <-settled:
		gate.mu.Lock()
		released := gate.released
		gate.mu.Unlock()
		if released {
			return nil
		}
		return errCatalogGateClosed
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (gate *catalogEnumerationGate) serve() {
	defer close(gate.done)
	for range catalogGateMaximumAttempts {
		connection, err := gate.listener.Accept()
		if err != nil {
			gate.mu.Lock()
			closed := gate.closed || gate.released
			if !closed {
				gate.serveErr = errors.Join(gate.serveErr, fmt.Errorf("accept catalog gate control: %w", err))
				gate.closeWithoutReleaseLocked()
			}
			gate.mu.Unlock()
			return
		}
		gate.mu.Lock()
		if gate.closed || gate.released {
			gate.mu.Unlock()
			_ = connection.Close()
			return
		}
		gate.active = connection
		gate.acceptedOnce.Do(func() { close(gate.accepted) })
		gate.mu.Unlock()
		released := gate.serveConnection(connection)
		gate.mu.Lock()
		if gate.active == connection {
			gate.active = nil
		}
		gate.mu.Unlock()
		_ = connection.Close()
		if released {
			return
		}
	}
	gate.mu.Lock()
	gate.serveErr = errors.Join(gate.serveErr, errors.New("catalog gate control attempt limit exceeded"))
	gate.closeWithoutReleaseLocked()
	gate.mu.Unlock()
	_ = gate.listener.Close()
}

func (gate *catalogEnumerationGate) serveConnection(connection net.Conn) bool {
	_ = connection.SetDeadline(time.Now().Add(catalogGateConnectionDeadline))
	document, err := io.ReadAll(io.LimitReader(connection, catalogGateMaximumBytes+2))
	if err != nil || len(document) > catalogGateMaximumBytes {
		return false
	}
	request, err := ownerprotocol.DecodeLine[catalogGateControlRequest](document)
	if err != nil || request.SchemaVersion != catalogGateControlSchema ||
		request.Identity != gate.identity ||
		(request.Action != catalogGateActionSnapshot && request.Action != catalogGateActionRelease) {
		return false
	}

	gate.mu.Lock()
	if gate.closed {
		gate.mu.Unlock()
		return false
	}
	response := catalogGateControlResponse{
		SchemaVersion:   catalogGateResponseSchema,
		Action:          request.Action,
		Outcome:         "observed",
		BlockedRequests: gate.blockedRequests,
		Released:        gate.released,
	}
	acceptedRelease := request.Action == catalogGateActionRelease &&
		gate.blockedRequests > 0 && !gate.released
	if acceptedRelease {
		gate.released = true
		response.Outcome = "released"
		response.Released = true
		close(gate.settled)
		_ = gate.listener.Close()
	}
	gate.mu.Unlock()
	if err := ownerprotocol.WriteLineDocument(connection, response); err != nil {
		if acceptedRelease {
			gate.mu.Lock()
			gate.serveErr = errors.Join(gate.serveErr, fmt.Errorf("publish catalog gate release response: %w", err))
			gate.mu.Unlock()
		}
		return acceptedRelease
	}
	return acceptedRelease
}

func (gate *catalogEnumerationGate) closeWithoutReleaseLocked() {
	if gate.closed || gate.released {
		return
	}
	gate.closed = true
	close(gate.settled)
}

func (gate *catalogEnumerationGate) Close() error {
	if gate == nil {
		return nil
	}
	gate.closeOnce.Do(func() {
		gate.mu.Lock()
		gate.closeWithoutReleaseLocked()
		active := gate.active
		gate.mu.Unlock()
		listenerErr := gate.listener.Close()
		var connectionErr error
		if active != nil {
			connectionErr = active.Close()
		}
		<-gate.done
		gate.mu.Lock()
		serveErr := gate.serveErr
		gate.mu.Unlock()
		if errors.Is(listenerErr, net.ErrClosed) {
			listenerErr = nil
		}
		if errors.Is(connectionErr, net.ErrClosed) {
			connectionErr = nil
		}
		gate.closeErr = errors.Join(listenerErr, connectionErr, serveErr)
	})
	return gate.closeErr
}
