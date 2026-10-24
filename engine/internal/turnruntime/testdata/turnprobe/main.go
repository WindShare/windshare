// This opt-in probe exercises the production native provider against Chromium.
// It is separate from ordinary tests, which need no external account.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/pionpeer"
	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/connectivity/turncapacity"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/connectivity/turnroute"
)

const (
	probePayloadBytes        = 512 * 1024
	maximumProbePayloadBytes = 64 * 1024 * 1024
	probeChunkBytes          = 16 * 1024
	probeTimeout             = 45 * time.Second
	routeProbe               = "192.0.2.1:9"
)

type probe struct {
	mu           sync.Mutex
	source       *trialTURN
	issued       map[byte]*turnpath.Lease
	sockets      *socketauthority.Authority
	turn         *turncapacity.Owner
	address      netip.Addr
	next         byte
	paths        map[byte]*pionpeer.Connection
	payloadBytes int
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	payloadBytes := flag.Int("bytes", probePayloadBytes, "Payload bytes per transfer")
	flag.Parse()
	if *payloadBytes < 1 || *payloadBytes > maximumProbePayloadBytes {
		return errors.New("probe payload must be between 1 byte and 64 MiB")
	}
	source, err := trialIssuer()
	if err != nil {
		return err
	}
	defer source.Close()
	route, err := net.Dial("udp4", routeProbe)
	if err != nil {
		return err
	}
	address := route.LocalAddr().(*net.UDPAddr).AddrPort().Addr()
	_ = route.Close()
	owner := &probe{source: source, sockets: socketauthority.New(socketauthority.Config{}),
		turn: turncapacity.NewPool().NewOwner(), address: address, paths: make(map[byte]*pionpeer.Connection), issued: make(map[byte]*turnpath.Lease), payloadBytes: *payloadBytes}
	defer owner.sockets.Close()
	defer func() {
		for _, connection := range owner.paths {
			_ = connection.Close()
		}
		for _, issued := range owner.issued {
			issued.Close()
		}
	}()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>TURN transport probe</title>"))
	})
	mux.HandleFunc("/start", owner.start)
	mux.HandleFunc("/answer", owner.answer)
	mux.HandleFunc("/stats", owner.stats)
	mux.HandleFunc("/close", owner.closePath)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	mux.HandleFunc("/stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		go func() { _ = server.Shutdown(context.Background()) }()
	})
	if err = json.NewEncoder(os.Stdout).Encode(map[string]any{"origin": "http://" + listener.Addr().String()}); err != nil {
		return err
	}
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (p *probe) start(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	var replaces [16]byte
	if raw := r.URL.Query().Get("replaces"); raw != "" {
		id, err := strconv.ParseUint(raw, 10, 8)
		if err != nil {
			http.Error(w, "invalid predecessor", http.StatusBadRequest)
			return
		}
		replaces[0] = byte(id)
	}
	p.mu.Lock()
	p.next++
	id := p.next
	p.mu.Unlock()
	issued, err := p.issue(ctx, id, replaces[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	p.mu.Lock()
	p.issued[id] = issued
	p.mu.Unlock()
	urls := issued.Relay().URLs()
	switch r.URL.Query().Get("transport") {
	case "udp":
		urls = []string{"turn:turn.cloudflare.com:3478?transport=udp"}
	case "tcp":
		urls = []string{"turn:turn.cloudflare.com:3478?transport=tcp"}
	case "tls":
		urls = []string{"turns:turn.cloudflare.com:443?transport=tcp"}
	case "profile":
	default:
		http.Error(w, "unknown TURN transport", http.StatusBadRequest)
		return
	}
	relay, err := iceconfig.NewRelay(iceconfig.Identity{Provider: "cloudflare", Account: "trial", Profile: r.URL.Query().Get("transport")},
		urls, issued.Relay().Credentials())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ice, err := iceconfig.New(nil, &relay, iceconfig.RelayCandidates)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	lease, err := p.sockets.Acquire([16]byte{1}, 1, [16]byte{id}, []netip.Addr{p.address})
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer lease.Close()
	reservation, err := p.turn.Acquire(ctx, turncapacity.Request{
		ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{id}, ReplacesPeerPathID: replaces,
		Service: relay.Identity(), Sockets: len(relay.URLs()),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	plan, err := turnroute.NewPlan(relay, nil)
	if err != nil {
		reservation.Close()
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	connection, err := pionpeer.NewPeerConnection(pion.Configuration{}, pionpeer.AttemptConfig{
		ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{id}, AttemptID: [16]byte{id}, NetworkGenerationID: 1,
		ICE: ice, TURN: &pionpeer.TURNConfig{Plan: plan, Reservation: reservation}, SocketLease: lease,
	})
	if err != nil {
		reservation.Close()
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	p.mu.Lock()
	p.paths[id] = connection
	p.mu.Unlock()
	channel, err := connection.CreateDataChannel("turn-probe", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	channel.OnMessage(func(message pion.DataChannelMessage) {
		if string(message.Data) != "start" {
			return
		}
		go func() {
			payload := make([]byte, probeChunkBytes)
			for i := range payload {
				payload[i] = byte(i % 251)
			}
			for sent := 0; sent < p.payloadBytes; sent += len(payload) {
				if err := channel.Send(payload[:min(len(payload), p.payloadBytes-sent)]); err != nil {
					return
				}
			}
		}()
	})
	gathered := pion.GatheringCompletePromise(connection.PeerConnection)
	offer, err := connection.CreateOffer(nil)
	if err == nil {
		err = connection.SetLocalDescription(offer)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	select {
	case <-ctx.Done():
		http.Error(w, ctx.Err().Error(), http.StatusGatewayTimeout)
		return
	case <-gathered:
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": id, "offer": connection.LocalDescription(), "bytes": p.payloadBytes,
		"iceServers": []map[string]any{{"urls": urls, "username": issued.Relay().Credentials().Username(), "credential": issued.Relay().Credentials().Password()}},
	})
}

func (p *probe) path(r *http.Request) *pionpeer.Connection {
	id, _ := strconv.ParseUint(r.URL.Query().Get("id"), 10, 8)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paths[byte(id)]
}
func (p *probe) answer(w http.ResponseWriter, r *http.Request) {
	connection := p.path(r)
	if connection == nil {
		http.NotFound(w, r)
		return
	}
	var answer pion.SessionDescription
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&answer)
	if err == nil {
		err = connection.SetRemoteDescription(answer)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (p *probe) closePath(w http.ResponseWriter, r *http.Request) {
	connection := p.path(r)
	if connection == nil {
		http.NotFound(w, r)
		return
	}
	if err := connection.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resources, _ := connection.TURNResources()
	_ = json.NewEncoder(w).Encode(resources)
}
