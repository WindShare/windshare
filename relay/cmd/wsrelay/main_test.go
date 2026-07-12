package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/admission"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/relay/signaling"
)

func TestRunServesAndShutsDownGracefully(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-listen", "127.0.0.1:0"}, func(a net.Addr) { ready <- a }, t.Logf)
	}()

	var addr net.Addr
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("service did not become ready")
	}

	resp, err := http.Get(fmt.Sprintf("http://%s/v1/config", addr))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var cfg protocol.ServerConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.ProtocolVersions) == 0 || cfg.ProtocolVersions[0] != protocol.ProtocolVersion {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Limits.Admission.MaxConnections <= 0 ||
		cfg.Limits.Admission.MaxSharesPerSource <= 0 ||
		cfg.Limits.Timeouts.WebSocketKeepaliveMilliseconds <= 0 ||
		cfg.Limits.Timeouts.SenderReconnectGraceMilliseconds <= 0 ||
		cfg.Limits.MaxHeaderBytes <= 0 {
		t.Fatalf("effective limits were not advertised: %+v", cfg.Limits)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown should return nil: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("graceful shutdown did not complete")
	}
}

func TestRunWithCustomAdmissionPolicy(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-listen", "127.0.0.1:0",
			"-max-connections", "17",
			"-join-share-rate", "3",
			"-sender-grace", "7s",
			"-origins", "https://windshare.top",
		},
			func(a net.Addr) { ready <- a }, t.Logf)
	}()
	select {
	case addr := <-ready:
		resp, err := http.Get(fmt.Sprintf("http://%s/v1/config", addr))
		if err != nil {
			t.Fatal(err)
		}
		var cfg protocol.ServerConfig
		if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		if cfg.Limits.Admission.MaxConnections != 17 || cfg.Limits.Admission.JoinPerShare.PerSecond != 3 ||
			cfg.Limits.Timeouts.SenderReconnectGraceMilliseconds != 7000 {
			t.Fatalf("advertised policy = %+v", cfg.Limits.Admission)
		}
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("service did not become ready")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

func TestServerPolicyBuildsHardenedServer(t *testing.T) {
	policy := serverPolicy{
		readHeaderTimeout: 2 * time.Second,
		readTimeout:       3 * time.Second,
		idleTimeout:       4 * time.Second,
		maxHeaderBytes:    1234,
	}
	if err := policy.validate(); err != nil {
		t.Fatal(err)
	}
	srv := policy.newServer(http.NewServeMux(), nil)
	if srv.ReadHeaderTimeout != policy.readHeaderTimeout ||
		srv.ReadTimeout != policy.readTimeout ||
		srv.IdleTimeout != policy.idleTimeout ||
		srv.MaxHeaderBytes != policy.maxHeaderBytes {
		t.Fatalf("server = %+v", srv)
	}
}

func TestConnectionTrackerCoversTCPAndTransfersHijackedLease(t *testing.T) {
	cfg := admission.DefaultConfig()
	cfg.MaxConnections = 1
	controller, err := admission.NewController(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hub := signaling.NewHub(signaling.Config{Admission: controller})
	t.Cleanup(hub.Close)
	tracker := newConnectionTracker(hub)

	first, firstPeer := net.Pipe()
	defer first.Close()
	defer firstPeer.Close()
	_ = tracker.connContext(context.Background(), first)
	if snapshot := controller.Snapshot(); snapshot.Connections != 1 {
		t.Fatalf("accepted TCP connection = %+v", snapshot)
	}

	denied, deniedPeer := net.Pipe()
	defer deniedPeer.Close()
	_ = tracker.connContext(context.Background(), denied)
	if snapshot := controller.Snapshot(); snapshot.Connections != 1 {
		t.Fatalf("denied TCP connection changed accounting: %+v", snapshot)
	}
	tracker.connState(first, http.StateClosed)
	if snapshot := controller.Snapshot(); snapshot.Connections != 0 {
		t.Fatalf("closed TCP connection leaked accounting: %+v", snapshot)
	}

	hijacked, hijackedPeer := net.Pipe()
	defer hijacked.Close()
	defer hijackedPeer.Close()
	_ = tracker.connContext(context.Background(), hijacked)
	tracker.mu.Lock()
	lease := tracker.leases[hijacked]
	tracker.mu.Unlock()
	tracker.connState(hijacked, http.StateHijacked)
	if snapshot := controller.Snapshot(); snapshot.Connections != 1 {
		t.Fatalf("hijack released transferred lease: %+v", snapshot)
	}
	lease.ReleaseIfTransferred()
	if snapshot := controller.Snapshot(); snapshot.Connections != 0 {
		t.Fatalf("transferred lease was not releasable: %+v", snapshot)
	}
}

func TestRunRejectsInvalidSecurityPolicyBeforeListening(t *testing.T) {
	tests := [][]string{
		{"-listen", "127.0.0.1:0", "-origins", "not-an-origin"},
		{"-listen", "127.0.0.1:0", "-register-ip-rate", "0"},
		{"-listen", "127.0.0.1:0", "-http-read-header-timeout", "0s"},
		{"-listen", "127.0.0.1:0", "-http-read-header-timeout", "5s", "-http-read-timeout", "1s"},
		{"-listen", "127.0.0.1:0", "-ws-keepalive-timeout", "0s"},
		{"-listen", "127.0.0.1:0", "-ws-role-timeout", "1ns"},
		{"-listen", "127.0.0.1:0", "-sender-grace", "1ns"},
		{"-listen", "127.0.0.1:0", "-max-frame-bytes", "65537"},
		{"-listen", "127.0.0.1:0", "-max-manifest-bytes", "16777217"},
		{"-listen", "127.0.0.1:0", "-max-manifest-bytes", "100", "-max-manifest-bytes-per-ip", "99"},
		{"-listen", "127.0.0.1:0", "-max-connections", "9007199254740992"},
		{"-listen", "127.0.0.1:0", "-http-max-header-bytes", "9007199254740992"},
	}
	for _, args := range tests {
		if err := run(context.Background(), args, nil, t.Logf); err == nil {
			t.Fatalf("invalid policy was accepted: %v", args)
		}
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	if err := run(context.Background(), []string{"-definitely-not-a-flag"}, nil, t.Logf); err == nil {
		t.Fatal("invalid flag should return an error")
	}
	if err := run(context.Background(), []string{"-listen", "999.999.999.999:1"}, nil, t.Logf); err == nil {
		t.Fatal("invalid listen address should return an error")
	}
}
