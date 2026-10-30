package senderrelay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/windshare/windshare/transport/relayv2"
)

func TestCandidateRejectsAmbiguousAddressesAndInvalidHeaderValues(t *testing.T) {
	for _, candidate := range []Candidate{
		{}, {PublicURL: "https://user:password@relay.example"},
		{PublicURL: "https://relay.example#private"},
		{PublicURL: "https://relay.example?%73hare_id=preselected"},
		{PublicURL: "https://relay.example?share_id=x;another=y"},
		{PublicURL: "https://relay.example", AccessKey: "token\r\nInjected: value"},
		{PublicURL: "https://relay.example", AccessKey: "with spaces"},
		{PublicURL: "https://relay.example", AccessKey: strings.Repeat("x", MaxAccessKeyBytes+1)},
	} {
		if candidate.Validate() == nil {
			t.Fatal("invalid candidate accepted")
		}
	}
	if _, err := PublicURLs([]Candidate{{PublicURL: "https://relay.example"}, {PublicURL: "wss://RELAY.example:443/"}}); err == nil {
		t.Fatal("competing aliases accepted")
	}
	candidate := Candidate{PublicURL: "https://relay.example?region=one", AccessKey: "private-token"}
	urls, err := PublicURLs([]Candidate{candidate})
	if err != nil || len(urls) != 1 || urls[0] != candidate.PublicURL {
		t.Fatalf("public addresses=%v error=%v", urls, err)
	}
	encoded, err := json.Marshal(candidate)
	if err != nil || strings.Contains(string(encoded), candidate.AccessKey) {
		t.Fatal("generic candidate serialization exposed registration authority")
	}
}

func TestRegistrationCredentialCannotFollowRedirect(t *testing.T) {
	requests := make(chan string, 2)
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- r.Header.Get("Authorization"):
		default:
		}
		http.Redirect(w, r, "/other-deployment/v2/ws", http.StatusTemporaryRedirect)
	}))
	config := newSenderRelayTestConfig(t, nil, nil, nil)
	config.Candidate = Candidate{PublicURL: "https://relay.example", AccessKey: "bound-token"}
	dial := config.Candidate.registrationDialOptions(0, false)
	dial.HTTPClient.Transport = server.Client().Transport
	_, err := relayv2.DialSender(t.Context(), relayv2.SenderConfig{
		RelayBaseURL: config.Candidate.PublicURL, Init: config.Fresh, SenderPrivateKey: config.PrivateKey,
		ResumeToken: config.ResumeToken, Descriptor: config.Descriptor,
		Dial: dial,
	})
	if err == nil {
		t.Fatal("redirect accepted")
	}
	if len(requests) != 1 || <-requests != "Bearer bound-token" {
		t.Fatal("credential was omitted or followed a redirect")
	}
}

func TestStopDoesNotUseDeploymentRegistrationAuthority(t *testing.T) {
	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	endpoint := &senderRelayTestEndpoint{accept: func(context.Context) (*relayv2.Channel, error) { return nil, nil }}
	config := newSenderRelayTestConfig(t, endpoint, nil, nil)
	config.Candidate = Candidate{PublicURL: server.URL, AccessKey: "register-only"}
	lifecycle, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Cleanup(t.Context()); err == nil {
		t.Fatal("expected unavailable STOP")
	}
	if len(requests) != 1 || <-requests != "" {
		t.Fatal("STOP used deployment registration authority")
	}
}
