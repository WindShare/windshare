// tcpserver is a bounded local browser capability fixture. It never enables a
// production transport capability or contacts an external service.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
)

const operationLimit = 10 * time.Second

type session struct {
	pc        *pion.PeerConnection
	mux       *ice.TCPMuxDefault
	forwarder net.Listener
}

func (s session) close() {
	_ = s.pc.Close()
	_ = s.mux.Close()
	if s.forwarder != nil {
		_ = s.forwarder.Close()
	}
}
func localAddress(ipv6 bool) string {
	if !ipv6 {
		return "127.0.0.1"
	}
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsLoopback() {
				return ip.String()
			}
		}
	}
	return ""
}
func main() {
	var mu sync.Mutex
	sessions := make(map[string]session)
	handler := http.NewServeMux()
	handler.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>WindShare TCP capability fixture</title>"))
	})
	handler.HandleFunc("/offer", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), operationLimit)
		defer cancel()
		ipv6 := r.URL.Query().Get("family") == "6"
		address := localAddress(ipv6)
		if address == "" {
			http.Error(w, "no eligible local IPv6 address", http.StatusServiceUnavailable)
			return
		}
		network := pion.NetworkTypeTCP4
		listenNetwork := "tcp4"
		if ipv6 {
			network = pion.NetworkTypeTCP6
			listenNetwork = "tcp6"
		}
		listener, err := net.Listen(listenNetwork, net.JoinHostPort(address, "0"))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		mux := ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: listener})
		var settings pion.SettingEngine
		settings.SetNetworkTypes([]pion.NetworkType{network})
		settings.SetIncludeLoopbackCandidate(true)
		settings.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryOnly)
		settings.SetIPFilter(func(ip net.IP) bool { return ip.String() == address })
		settings.SetICETCPMux(mux)
		settings.DisableActiveTCP(true)
		var forwarder net.Listener
		if r.URL.Query().Get("mapped") == "1" {
			forwarder, err = net.Listen(listenNetwork, net.JoinHostPort(address, "0"))
			if err != nil {
				_ = mux.Close()
				http.Error(w, err.Error(), 500)
				return
			}
			local := listener.Addr().(*net.TCPAddr).AddrPort()
			external := forwarder.Addr().(*net.TCPAddr).AddrPort()
			settings.SetICEProviderConfig(ice.ProviderConfig{TCPMappedMux: mappedListener{mux, local}, MappedTCPEndpoints: []ice.MappedEndpoint{{Local: local, External: external}}})
			go forwardTCP(forwarder, listener.Addr())
		}
		pc, err := pion.NewAPI(pion.WithSettingEngine(settings)).NewPeerConnection(pion.Configuration{})
		if err != nil {
			_ = mux.Close()
			http.Error(w, err.Error(), 500)
			return
		}
		current := session{pc, mux, forwarder}
		retained := false
		defer func() {
			if !retained {
				current.close()
			}
		}()
		var offer pion.SessionDescription
		if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&offer); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		pc.OnDataChannel(func(channel *pion.DataChannel) {
			channel.OnMessage(func(message pion.DataChannelMessage) { _ = channel.SendText("tcp-proof:" + string(message.Data)) })
		})
		if err = pc.SetRemoteDescription(offer); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		gathering := pion.GatheringCompletePromise(pc)
		if err = pc.SetLocalDescription(answer); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		select {
		case <-ctx.Done():
			http.Error(w, ctx.Err().Error(), 504)
			return
		case <-gathering:
		}
		id := listener.Addr().String()
		mu.Lock()
		sessions[id] = current
		mu.Unlock()
		retained = true
		time.AfterFunc(2*operationLimit, func() {
			mu.Lock()
			s, ok := sessions[id]
			delete(sessions, id)
			mu.Unlock()
			if ok {
				s.close()
			}
		})
		w.Header().Set("Content-Type", "application/json")
		description := *pc.LocalDescription()
		if forwarder != nil {
			description.SDP = mappedOnlySDP(description.SDP)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "answer": description})
	})
	handler.HandleFunc("/result", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current, ok := sessions[r.URL.Query().Get("id")]
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		pair, err := current.pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
		if err != nil || pair == nil {
			http.Error(w, "no selected pair", 409)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": pair.Local.Protocol.String(), "localType": pair.Local.Typ.String(), "remoteType": pair.Remote.Typ.String(), "localAddress": pair.Local.Address, "localPort": pair.Local.Port, "remoteAddress": pair.Remote.Address, "remotePort": pair.Remote.Port})
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: operationLimit}
	fmt.Println("http://" + listener.Addr().String())
	time.AfterFunc(2*time.Minute, func() { _ = server.Close() })
	_ = server.Serve(listener)
	mu.Lock()
	defer mu.Unlock()
	for _, current := range sessions {
		current.close()
	}
}
