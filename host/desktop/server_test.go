package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestControlAuthenticatesAndRecoversAfterLostCreateReply(t *testing.T) {
	a, _, starts := fixture(t)
	created := make(chan struct{})
	start := a.deps.start
	a.deps.start = func(ctx context.Context, request engine.ShareRequest) (shareTask, error) {
		task, err := start(ctx, request)
		close(created)
		return task, err
	}
	server, err := listen(a, "secret")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	address := "ws://" + server.address + "/control"
	for _, headers := range []http.Header{nil, {"Authorization": []string{"Bearer wrong"}}, {"Authorization": []string{"Bearer secret"}, "Origin": []string{"https://foreign.example"}}} {
		conn, response, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPHeader: headers})
		if conn != nil {
			_ = conn.CloseNow()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatal("unauthenticated control accepted")
		}
	}
	options := &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer secret"}}}
	conn, _, err := websocket.Dial(ctx, address, options)
	if err != nil {
		t.Fatal(err)
	}
	var initial Message
	if err := wsjson.Read(ctx, conn, &initial); err != nil {
		t.Fatal(err)
	}
	if initial.Snapshot.SessionID != "session" {
		t.Fatal("missing reconnect snapshot")
	}
	if err := wsjson.Write(ctx, conn, createRequest()); err != nil {
		t.Fatal(err)
	}
	// Wait for admission, then discard the socket without reading its reply.
	select {
	case <-created:
	case <-ctx.Done():
		t.Fatal("host did not admit creation")
	}
	_ = conn.CloseNow()
	var reply Message
	conn, _, err = websocket.Dial(ctx, address, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	if err := wsjson.Read(ctx, conn, &initial); err != nil {
		t.Fatal(err)
	}
	if len(initial.Snapshot.Shares) != 1 {
		t.Fatal("disconnect released task")
	}
	if err := wsjson.Write(ctx, conn, createRequest()); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(ctx, conn, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error != nil || starts.Load() != 1 {
		t.Fatal("retry duplicated share")
	}
}

func TestSlowObserversCannotBlockAndSnapshotsRepairDroppedEvents(t *testing.T) {
	a, task, _ := fixture(t)
	a.execute(createRequest())
	admitted(t, a)
	queue, unsubscribe := a.subscribe()
	defer unsubscribe()
	event, err := appobs.NewRelayAvailability(1, 1, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	for range notificationCapacity * 3 {
		a.publishObservation("operation", engine.Observation{TaskID: "engine:1", Event: event})
	}
	if len(queue.evidence) != notificationCapacity {
		t.Fatal("notification queue is not bounded")
	}
	task.mu.Lock()
	task.state.RelayAvailability = &event
	task.mu.Unlock()
	snapshot := a.snapshot()
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) == 0 || snapshot.Shares[0].Relay == nil {
		t.Fatal("snapshot requires delivered observations")
	}
}
