package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReleaseClientAndFetchHTTPS(t *testing.T) {
	t.Parallel()

	var redirectTargetHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /directory", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("release"))
	})
	mux.HandleFunc("GET /moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	mux.HandleFunc("GET /target", func(w http.ResponseWriter, _ *http.Request) {
		redirectTargetHits.Add(1)
		_, _ = w.Write([]byte("redirected"))
	})
	mux.HandleFunc("GET /error", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()

	client := releaseClient()
	if client.Timeout != releaseTimeout {
		t.Fatalf("releaseClient timeout = %v, want %v", client.Timeout, releaseTimeout)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	client.Transport = transport
	defer client.CloseIdleConnections()

	data, err := fetchHTTPS(t.Context(), client, server.URL+"/directory", int64(len("release")))
	if err != nil || string(data) != "release" {
		t.Fatalf("fetchHTTPS exact limit = (%q, %v), want (%q, nil)", data, err, "release")
	}

	if _, err := fetchHTTPS(t.Context(), client, server.URL+"/directory", int64(len("release")-1)); err == nil || !strings.Contains(err.Error(), "response too large") {
		t.Fatalf("oversized response err = %v, want response too large", err)
	}

	if _, err := fetchHTTPS(t.Context(), client, server.URL+"/moved", 64); err == nil {
		t.Fatal("fetchHTTPS followed a redirect")
	}
	if redirectTargetHits.Load() != 0 {
		t.Fatalf("redirect target received %d requests, want 0", redirectTargetHits.Load())
	}

	if _, err := fetchHTTPS(t.Context(), client, server.URL+"/error", 64); err == nil {
		t.Fatal("fetchHTTPS accepted HTTP 503 response")
	}

	for _, invalid := range []string{
		"http://authority.example/.well-known/windshare-network",
		"https:///missing-host",
		"https://user@authority.example/.well-known/windshare-network",
		server.URL + "/directory#fragment",
	} {
		if _, err := fetchHTTPS(t.Context(), client, invalid, 64); err == nil {
			t.Fatalf("fetchHTTPS(%q) succeeded, want rejection", invalid)
		}
	}

	insecureTransport := http.DefaultTransport.(*http.Transport).Clone()
	insecureTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	insecureClient := releaseClient()
	insecureClient.Transport = insecureTransport
	defer insecureClient.CloseIdleConnections()
	if _, err := fetchHTTPS(t.Context(), insecureClient, server.URL+"/directory", 64); err == nil {
		t.Fatal("fetchHTTPS accepted an unverified TLS chain")
	}
}
