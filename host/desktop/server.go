package desktop

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	maximumClients      = 8
	maximumMessageBytes = 256 * 1024
	refreshInterval     = time.Second
	writeTimeout        = 2 * time.Second
)

type server struct {
	app         *Application
	token       string
	address     string
	http        *http.Server
	listener    net.Listener
	mu          sync.Mutex
	connections map[*websocket.Conn]struct{}
	closing     bool
	workers     sync.WaitGroup
	done        chan struct{}
}

func listen(app *Application, token string) (*server, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &server{app: app, token: token, address: listener.Addr().String(), listener: listener,
		connections: make(map[*websocket.Conn]struct{}), done: make(chan struct{})}
	s.http = &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: writeTimeout}
	go func() {
		defer close(s.done)
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			app.deps.log("host.listener_failed", errorText(err))
		}
	}()
	return s, nil
}

func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	// Native clients use an Authorization header. Browser-origin handshakes and
	// alternate Host values are rejected before upgrading, including loopback DNS rebinding.
	if r.Method != http.MethodGet || r.URL.Path != "/control" || r.URL.RawQuery != "" ||
		r.Host != s.address || r.Header.Get("Origin") != "" ||
		subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
		http.Error(w, "unauthorized local control request", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	if s.closing || len(s.connections) >= maximumClients {
		s.mu.Unlock()
		http.Error(w, "host unavailable", http.StatusServiceUnavailable)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.mu.Unlock()
		return
	}
	s.connections[conn] = struct{}{}
	s.workers.Add(1)
	s.mu.Unlock()
	defer func() {
		_ = conn.CloseNow()
		s.mu.Lock()
		delete(s.connections, conn)
		s.mu.Unlock()
		s.workers.Done()
	}()
	s.control(conn)
}

func readRequests(ctx context.Context, conn *websocket.Conn, requests chan<- Request) {
	defer close(requests)
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if kind != websocket.MessageText {
			return
		}
		var request Request
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			return
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF || len(request.ID) == 0 || len(request.ID) > 128 {
			return
		}
		select {
		case requests <- request:
		case <-ctx.Done():
			return
		}
	}
}

func (s *server) control(conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	conn.SetReadLimit(maximumMessageBytes)
	requests := make(chan Request)
	readerDone := make(chan struct{})
	go func() { defer close(readerDone); readRequests(ctx, conn, requests) }()
	defer func() { cancel(); _ = conn.CloseNow(); <-readerDone }()
	notifications, unsubscribe := s.app.subscribe()
	defer unsubscribe()
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	snapshot := s.app.snapshot()
	if !writeMessage(ctx, conn, Message{Version: ProtocolVersion, Snapshot: &snapshot}) {
		return
	}
	for {
		var message Message
		select {
		case request, ok := <-requests:
			if !ok {
				return
			}
			message = s.app.execute(request)
		case message = <-notifications:
		case <-ticker.C:
			snapshot := s.app.snapshot()
			message = Message{Version: ProtocolVersion, Snapshot: &snapshot}
		}
		if !writeMessage(ctx, conn, message) {
			return
		}
	}
}

func writeMessage(ctx context.Context, conn *websocket.Conn, message Message) bool {
	data, err := message.bytes()
	if err != nil {
		return false
	}
	writeContext, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(writeContext, websocket.MessageText, data) == nil
}

func (s *server) close() {
	s.mu.Lock()
	s.closing = true
	for conn := range s.connections {
		_ = conn.CloseNow()
	}
	s.mu.Unlock()
	_ = s.http.Close()
	<-s.done
	s.workers.Wait()
}
