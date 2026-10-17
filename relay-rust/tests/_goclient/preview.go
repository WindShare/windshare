package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/coder/websocket"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type previewStatus struct {
	Boot    string `json:"boot"`
	Alarm   int64  `json:"alarm"`
	Now     int64  `json:"now"`
	Sockets []struct {
		Tags            []string `json:"tags"`
		State           int      `json:"state"`
		AttachmentBytes int      `json:"attachmentBytes"`
	} `json:"sockets"`
}

func cloudflare(endpoint string, idle time.Duration) {
	require(idle >= 12*time.Second && idle <= 30*time.Second, "idle must leave room within the 45-second silence deadline")
	require(os.Getenv("WINDSHARE_RELAY_REGISTRATION_SECRET") != "", "registration secret required")
	require(os.Getenv("WINDSHARE_RELAY_PREVIEW_SECRET") != "", "Preview test secret required")
	expireIdleHandshake(endpoint)
	f := newFixture()
	unauthorized, _ := dialWithCredential(endpoint, f.init.Registration.ShareID, false)
	send(unauthorized, f.init)
	refusal(unauthorized, v2.ErrorInvalidProof)
	_ = unauthorized.CloseNow()
	rejectInvalidProofs(endpoint, f)

	sender := register(endpoint, f, false)
	defer sender.CloseNow()
	before := status(endpoint, f)
	require(len(before.Sockets) == 1 && before.Sockets[0].AttachmentBytes > 0, "missing live attachment")
	require(before.Sockets[0].AttachmentBytes <= 15872, "attachment exceeds kernel budget")
	time.Sleep(idle)
	after := status(endpoint, f)
	require(before.Boot != after.Boot, "normal hibernation was not observed; constructor identity unchanged")
	require(reflect.DeepEqual(before.Sockets, after.Sockets), "hibernation changed connection identity or attachments")
	for _, nonce := range []uint64{2, 3} {
		response, err := control(endpoint, f, http.MethodGet, "route-mismatch")
		check(err)
		_ = response.Body.Close()
		require(response.StatusCode == http.StatusBadRequest, "alternate endpoint must be refused")
		probe(sender, nonce)
	}
	fmt.Printf("PASS hibernation: share=%s before=%s after=%s attachment_bytes=%d\n",
		base64.RawURLEncoding.EncodeToString(f.init.Registration.ShareID[:]), before.Boot, after.Boot, after.Sockets[0].AttachmentBytes)

	reset(endpoint, f)
	closed(sender)
	_ = sender.CloseNow()
	sender = register(endpoint, f, true)
	defer sender.CloseNow()
	fmt.Println("PASS fault restart: fresh signed RESUME without deployment credential")

	stop(endpoint, f)
	reset(endpoint, f)
	for _, mode := range []v2.RegistrationMode{v2.RegistrationFresh, v2.RegistrationResume} {
		conn, _ := dialWithCredential(endpoint, f.init.Registration.ShareID, mode == v2.RegistrationFresh)
		init := f.init
		init.Registration.Mode = mode
		send(conn, init)
		if mode == v2.RegistrationResume {
			send(conn, v2.ResumeCredential{Token: f.token})
		}
		refusal(conn, v2.ErrorStopped)
		_ = conn.CloseNow()
	}
	stop(endpoint, f)
	fmt.Println("PASS permanent STOP: fresh REGISTER and RESUME refused after object reset; duplicate STOP acknowledged")
}

func expireIdleHandshake(endpoint string) {
	// Check the persisted absolute timestamp before waiting, so a platform SDK
	// accidentally interpreting it as a relative delay cannot escape detection.
	idleFixture := newFixture()
	silent, _ := dial(endpoint, idleFixture.init.Registration.ShareID)
	defer silent.CloseNow()
	snapshot := status(endpoint, idleFixture)
	const firstFrameTimeout = 10 * time.Second
	require(snapshot.Alarm >= snapshot.Now && snapshot.Alarm <= snapshot.Now+firstFrameTimeout.Milliseconds(),
		fmt.Sprintf("idle handshake Alarm is not an imminent absolute deadline: alarm=%d cloud_now=%d", snapshot.Alarm, snapshot.Now))
	// No subsequent inbound event drives this timeout: the production Alarm
	// must close it. Allow dispatch delays during platform maintenance/failover.
	const alarmDispatchTimeout = 70 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), alarmDispatchTimeout)
	defer cancel()
	_, _, err := silent.Read(ctx)
	require(err != nil && ctx.Err() == nil, "Alarm did not expire an idle handshake")
	fmt.Println("PASS Alarm: idle handshake expired without client traffic")
}

func probe(connection *websocket.Conn, nonce uint64) {
	send(connection, v2.ConnectionProbe{Nonce: nonce})
	ack, err := v2.ParseConnectionProbeAck(read(connection))
	check(err)
	require(ack.Nonce == nonce, "post-hibernation heartbeat mismatch")
}

func control(endpoint string, f fixture, method, action string) (*http.Response, error) {
	base := strings.TrimSuffix(endpoint, v2.WebSocketPath)
	base = strings.Replace(base, "wss:", "https:", 1)
	base = strings.Replace(base, "ws:", "http:", 1)
	url := base + "/__test/" + action + "?share_id=" + base64.RawURLEncoding.EncodeToString(f.init.Registration.ShareID[:])
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+os.Getenv("WINDSHARE_RELAY_PREVIEW_SECRET"))
	// Read the bounded response before returning so the request's context stays alive.
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	_ = response.Body.Close()
	response.Body = io.NopCloser(strings.NewReader(string(body)))
	return response, err
}

func status(endpoint string, f fixture) previewStatus {
	response, err := control(endpoint, f, http.MethodGet, "status")
	check(err)
	defer response.Body.Close()
	require(response.StatusCode == http.StatusOK, fmt.Sprintf("status HTTP %d", response.StatusCode))
	var value previewStatus
	check(json.NewDecoder(response.Body).Decode(&value))
	return value
}

func reset(endpoint string, f fixture) {
	before := status(endpoint, f)
	response, err := control(endpoint, f, http.MethodPost, "reset")
	if err == nil {
		defer response.Body.Close()
		require(response.StatusCode >= 500, fmt.Sprintf("reset did not abort: HTTP %d", response.StatusCode))
	}
	// A request error alone does not establish that the fault was injected.
	after := status(endpoint, f)
	require(after.Boot != before.Boot, "reset did not reconstruct the object")
}
