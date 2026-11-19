package relayv2

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestDialWrapsHTTPUpgradeRejection(t *testing.T) {
	fixture := newClientFixture(t)
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUpgradeRequired, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "nope", status)
			}))
			t.Cleanup(server.Close)
			assertDialError(t, fixture, server.URL, true, func(t *testing.T, err error) {
				t.Helper()
				var dialErr *WebSocketDialError
				if !errors.As(err, &dialErr) || strings.Contains(err.Error(), "101") || strings.Contains(err.Error(), "nope") {
					t.Fatalf("upgrade rejection = %v", err)
				}
			})
		})
	}
}

func TestDialWrapsCloseAfterHandshake(t *testing.T) {
	fixture := newClientFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		_ = connection.Close(websocket.StatusNormalClosure, "bye")
	}))
	t.Cleanup(server.Close)
	assertDialError(t, fixture, server.URL, false, func(t *testing.T, err error) {
		t.Helper()
		var socketErr *SocketError
		if !errors.As(err, &socketErr) || !errors.Is(err, ErrClosed) ||
			strings.Contains(err.Error(), "bye") || strings.Contains(err.Error(), "close frame") {
			t.Fatalf("close after handshake = %v", err)
		}
	})
}

func TestReceiverAndStopSuggestBaseForDirect404(t *testing.T) {
	fixture := newClientFixture(t)
	client := &http.Client{Transport: relayUpgradeRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{Request: req, StatusCode: http.StatusNotFound, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	const address = "https://relay.example/team%2Fone/v2/ws/?token=secret-canary"
	const wantBase = "https://relay.example/team%2Fone"
	receiver, err := DialReceiver(t.Context(), ReceiverConfig{
		RelayBaseURL: address, ShareID: fixture.fresh.ShareID, Dial: DialOptions{HTTPClient: client},
	})
	if receiver != nil || !routeHintIs(err, wantBase) || strings.Contains(fmt.Sprint(err), "secret-canary") {
		t.Fatalf("JOIN 404 = (%v, %v)", receiver, err)
	}
	if err := Stop(t.Context(), StopConfig{
		RelayBaseURL: address, ShareID: fixture.fresh.ShareID, ShareInstance: fixture.fresh.ShareInstance,
		PKHash: fixture.fresh.PKHash, StopID: v2.StopID{1}, SenderPrivateKey: fixture.privateKey,
		Dial: DialOptions{HTTPClient: client},
	}); !routeHintIs(err, wantBase) || strings.Contains(fmt.Sprint(err), "secret-canary") {
		t.Fatalf("STOP 404 = %v", err)
	}
}

func assertDialError(t *testing.T, fixture clientFixture, base string, registrationNotSent bool, check func(*testing.T, error)) {
	t.Helper()
	_, err := DialSender(t.Context(), SenderConfig{
		RelayBaseURL: base, Init: fixture.fresh, SenderPrivateKey: fixture.privateKey,
		Descriptor: fixture.descriptor,
	})
	if _, unsent := errors.AsType[*RegistrationNotSentError](err); unsent != registrationNotSent {
		t.Fatalf("REGISTER delivery state = %v unsent=%t want %t", err, unsent, registrationNotSent)
	}
	check(t, err)
	_, err = DialReceiver(t.Context(), ReceiverConfig{RelayBaseURL: base, ShareID: fixture.fresh.ShareID})
	check(t, err)
	err = Stop(t.Context(), StopConfig{
		RelayBaseURL: base, ShareID: fixture.fresh.ShareID, ShareInstance: fixture.fresh.ShareInstance,
		PKHash: fixture.fresh.PKHash, StopID: v2.StopID{1}, SenderPrivateKey: fixture.privateKey,
	})
	check(t, err)
}

func routeHintIs(err error, want string) bool {
	hint, ok := errors.AsType[*WebSocketRouteError](err)
	return ok && hint.BaseHint.URL() == want
}
