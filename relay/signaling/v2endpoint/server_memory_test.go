package v2endpoint

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/relay/signaling/v2route"
	"github.com/windshare/windshare/transport/relayv2"
)

type sequenceReader struct {
	mu   sync.Mutex
	next byte
}

func (reader *sequenceReader) Read(destination []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	for index := range destination {
		destination[index] = reader.next
		reader.next++
		if reader.next == 0 {
			reader.next = 1
		}
	}
	return len(destination), nil
}

type memoryTombstoneStore struct {
	mu      sync.Mutex
	records []v2route.Tombstone
}

func (store *memoryTombstoneStore) Load(context.Context) ([]v2route.Tombstone, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]v2route.Tombstone(nil), store.records...), nil
}

func (store *memoryTombstoneStore) Commit(
	_ context.Context,
	record v2route.Tombstone,
) (v2route.CommitOutcome, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, existing := range store.records {
		if existing.ShareID != record.ShareID {
			continue
		}
		if existing == record {
			return v2route.CommitCommitted, nil
		}
		return v2route.CommitUnknown, v2route.ErrTombstoneConflict
	}
	store.records = append(store.records, record)
	return v2route.CommitCommitted, nil
}

type memoryMessage struct {
	kind websocket.MessageType
	data []byte
}

type memoryStream struct {
	mu         sync.Mutex
	messages   chan memoryMessage
	closed     bool
	closeError websocket.CloseError
}

type memorySocket struct {
	inbound  *memoryStream
	outbound *memoryStream
	limit    atomic.Int64
}

type failNthWriteSocket struct {
	BinaryConnection
	mu     sync.Mutex
	writes int
	failAt int
	err    error
}

func (socket *failNthWriteSocket) Write(
	ctx context.Context,
	kind websocket.MessageType,
	data []byte,
) error {
	socket.mu.Lock()
	socket.writes++
	fail := socket.writes == socket.failAt
	socket.mu.Unlock()
	if fail {
		return socket.err
	}
	return socket.BinaryConnection.Write(ctx, kind, data)
}

func newMemorySocketPair() (*memorySocket, *memorySocket) {
	left := &memoryStream{messages: make(chan memoryMessage, 2_048)}
	right := &memoryStream{messages: make(chan memoryMessage, 2_048)}
	return &memorySocket{inbound: left, outbound: right},
		&memorySocket{inbound: right, outbound: left}
}

func (socket *memorySocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case message, open := <-socket.inbound.messages:
		if !open {
			return 0, nil, socket.inbound.closeError
		}
		return message.kind, bytes.Clone(message.data), nil
	}
}

func (socket *memorySocket) Write(ctx context.Context, kind websocket.MessageType, data []byte) error {
	socket.outbound.mu.Lock()
	defer socket.outbound.mu.Unlock()
	if socket.outbound.closed {
		return io.ErrClosedPipe
	}
	message := memoryMessage{kind: kind, data: bytes.Clone(data)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case socket.outbound.messages <- message:
		return nil
	}
}

func (socket *memorySocket) Close(code websocket.StatusCode, reason string) error {
	socket.outbound.mu.Lock()
	defer socket.outbound.mu.Unlock()
	if socket.outbound.closed {
		return nil
	}
	// Channel close is ordered behind successful sends, matching the WebSocket
	// guarantee that a close frame cannot overtake earlier data frames.
	socket.outbound.closeError = websocket.CloseError{Code: code, Reason: reason}
	socket.outbound.closed = true
	close(socket.outbound.messages)
	return nil
}

func (socket *memorySocket) SetReadLimit(limit int64) { socket.limit.Store(limit) }

func TestMemorySocketDeliversQueuedFrameBeforeClose(t *testing.T) {
	local, remote := newMemorySocketPair()
	want := []byte("queued-before-close")
	if err := local.Write(context.Background(), websocket.MessageBinary, want); err != nil {
		t.Fatal(err)
	}
	if err := local.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	kind, got, err := remote.Read(context.Background())
	if err != nil || kind != websocket.MessageBinary || !bytes.Equal(got, want) {
		t.Fatalf("queued frame = kind %d, data %q, error %v", kind, got, err)
	}
	_, _, err = remote.Read(context.Background())
	closeError, ok := err.(websocket.CloseError)
	if !ok || closeError.Code != websocket.StatusNormalClosure || closeError.Reason != "" {
		t.Fatalf("close = %#v, want normal closure with empty reason", err)
	}
}

func memoryServerDialer(server *Server) func(context.Context, string, http.Header) (relayv2.BinarySocket, error) {
	return func(context.Context, string, http.Header) (relayv2.BinarySocket, error) {
		client, relay := newMemorySocketPair()
		go func() { _ = server.Serve(context.Background(), relay) }()
		return client, nil
	}
}
