package harness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/protocol"
)

func TestHealthAndConfigRoutes(t *testing.T) {
	harness, err := New()
	if err != nil {
		t.Fatalf("create harness: %v", err)
	}
	t.Cleanup(func() {
		if err := harness.Close(); err != nil {
			t.Errorf("close harness: %v", err)
		}
	})

	health := httptest.NewRecorder()
	harness.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || health.Body.String() != "ok\n" {
		t.Fatalf("health response = %d %q, want 200 ok", health.Code, health.Body.String())
	}

	configResponse := httptest.NewRecorder()
	harness.Handler().ServeHTTP(configResponse, httptest.NewRequest(http.MethodGet, "/config", nil))
	if configResponse.Code != http.StatusOK {
		t.Fatalf("config status = %d, want 200", configResponse.Code)
	}
	var config PublicConfig
	if err := json.Unmarshal(configResponse.Body.Bytes(), &config); err != nil {
		t.Fatalf("decode config response: %v", err)
	}
	if config.SessionID != spikeSessionID.String() || config.MaxFrameSize != session.MaxFrameSize {
		t.Fatalf("config response does not expose WindShare contract: %+v", config)
	}
}

func TestSignalRouteRejectsMalformedAndWrongSessionMessages(t *testing.T) {
	harness, err := New()
	if err != nil {
		t.Fatalf("create harness: %v", err)
	}
	t.Cleanup(func() {
		if err := harness.Close(); err != nil {
			t.Errorf("close harness: %v", err)
		}
	})

	wrongSession := protocol.SessionID{8, 7, 6, 5, 4, 3, 2, 1}
	wrongEnvelope, err := protocol.Encode(protocol.NewSignal(
		wrongSession.String(),
		protocol.SignalKindOffer,
		json.RawMessage(`{"type":"offer","sdp":"v=0\\r\\n"}`),
	))
	if err != nil {
		t.Fatalf("encode wrong-session signal: %v", err)
	}
	answerEnvelope, err := protocol.Encode(protocol.NewSignal(
		spikeSessionID.String(),
		protocol.SignalKindAnswer,
		json.RawMessage(`{"type":"answer","sdp":"v=0\\r\\n"}`),
	))
	if err != nil {
		t.Fatalf("encode unexpected answer signal: %v", err)
	}

	tests := []struct {
		name string
		body []byte
		want string
	}{
		{name: "malformed JSON", body: []byte(`{"type":`), want: "decode WindShare signal"},
		{name: "wrong session", body: wrongEnvelope, want: "sessionId does not match"},
		{name: "unexpected kind", body: answerEnvelope, want: "unexpected signal kind answer"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/signal", strings.NewReader(string(test.body)))
			harness.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%q", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("body = %q, want substring %q", response.Body.String(), test.want)
			}
		})
	}
}

func TestExtractMaxMessageSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sdp  string
		want uint64
	}{
		{name: "CRLF", sdp: "v=0\r\na=max-message-size:262144\r\n", want: 262144},
		{name: "LF", sdp: "v=0\na=max-message-size:65536\n", want: 65536},
		{name: "missing", sdp: "v=0\r\n", want: 0},
		{name: "invalid", sdp: "a=max-message-size:not-a-number\r\n", want: 0},
		{name: "overflow", sdp: "a=max-message-size:18446744073709551616\r\n", want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := extractMaxMessageSize(test.sdp); got != test.want {
				t.Fatalf("extractMaxMessageSize() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestWaitForBufferedAmountLowIgnoresStaleWake(t *testing.T) {
	t.Parallel()

	wakes := make(chan struct{}, 2)
	wakes <- struct{}{}
	wakes <- struct{}{}
	amounts := []uint64{lowWaterMarkBytes + 1, lowWaterMarkBytes}
	reads := 0
	readAmount := func() uint64 {
		amount := amounts[reads]
		reads++
		return amount
	}
	deadline := make(chan time.Time)

	if !waitForBufferedAmountLow(wakes, deadline, readAmount, lowWaterMarkBytes) {
		t.Fatal("valid low-water wake was not accepted")
	}
	if reads != 2 {
		t.Fatalf("BufferedAmount reads = %d, want 2 after one stale and one valid wake", reads)
	}
}

func TestWaitForBufferedAmountLowTimesOutWithoutWake(t *testing.T) {
	t.Parallel()

	deadline := make(chan time.Time, 1)
	deadline <- time.Now()
	if waitForBufferedAmountLow(make(chan struct{}), deadline, func() uint64 {
		t.Fatal("BufferedAmount must not be read without a callback")
		return 0
	}, lowWaterMarkBytes) {
		t.Fatal("timeout unexpectedly reported a low-water observation")
	}
}
