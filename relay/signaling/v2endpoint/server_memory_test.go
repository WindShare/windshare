package v2endpoint

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

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

type memorySocket struct {
	inbound  chan memoryMessage
	outbound chan memoryMessage
	done     <-chan struct{}
	close    func()
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
	left := make(chan memoryMessage, 2_048)
	right := make(chan memoryMessage, 2_048)
	done := make(chan struct{})
	var once sync.Once
	closePair := func() { once.Do(func() { close(done) }) }
	return &memorySocket{inbound: left, outbound: right, done: done, close: closePair},
		&memorySocket{inbound: right, outbound: left, done: done, close: closePair}
}

func (socket *memorySocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-socket.done:
		return 0, nil, websocket.CloseError{Code: websocket.StatusNormalClosure}
	case message := <-socket.inbound:
		return message.kind, bytes.Clone(message.data), nil
	}
}

func (socket *memorySocket) Write(ctx context.Context, kind websocket.MessageType, data []byte) error {
	message := memoryMessage{kind: kind, data: bytes.Clone(data)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-socket.done:
		return io.ErrClosedPipe
	case socket.outbound <- message:
		return nil
	}
}

func (socket *memorySocket) Close(websocket.StatusCode, string) error {
	socket.close()
	return nil
}

func (socket *memorySocket) SetReadLimit(limit int64) { socket.limit.Store(limit) }

func memoryServerDialer(server *Server) func(context.Context, string, http.Header) (relayv2.BinarySocket, error) {
	return func(context.Context, string, http.Header) (relayv2.BinarySocket, error) {
		client, relay := newMemorySocketPair()
		go func() { _ = server.Serve(context.Background(), relay) }()
		return client, nil
	}
}
