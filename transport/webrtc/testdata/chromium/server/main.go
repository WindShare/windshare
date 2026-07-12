package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"sync"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/session"
	windwebrtc "github.com/windshare/windshare/transport/webrtc"
)

const (
	defaultAddress  = "127.0.0.1:17846"
	scenarioEnvName = "WINDSHARE_D1_CHROMIUM_SCENARIO"
	operationLimit  = 20 * time.Second

	scenarioHappy        = "happy"
	scenarioCancellation = "cancellation"
	scenarioRemoteClose  = "remote-close"
	scenarioMalformed    = "malformed-setting"
	actionCloseChannel   = "close-data-channel"
	invalidProtocol      = windwebrtc.ChannelProtocol + "-invalid"

	lowWaterBytes  uint64 = 256 * 1024
	highWaterBytes uint64 = 1024 * 1024
	maximumBursts         = 64

	clientProbeMarker    = 0x61
	clientBurstMarker    = 0x62
	clientFinishedMarker = 0x63
	serverProbeMarker    = 0x71
	serverBurstMarker    = 0x72
	serverFinishedMarker = 0x73
	serverTerminalMarker = 0x74
	canceledSendMarker   = 0x75
	cancellationBarrier  = 0x76
	remoteCloseMarker    = 0x77
	terminalFrameBytes   = 257
)

//go:embed web/*
var assets embed.FS

type terminalFixture struct {
	TerminalIntent string `json:"terminalIntent"`
	TerminalAck    string `json:"terminalAck"`
}

type publicConfig struct {
	Scenario             string `json:"scenario"`
	ChannelLabel         string `json:"channelLabel"`
	ChannelProtocol      string `json:"channelProtocol"`
	InvalidProtocol      string `json:"invalidProtocol"`
	TerminalIntent       string `json:"terminalIntent"`
	TerminalAck          string `json:"terminalAck"`
	MaxFrameSize         int    `json:"maxFrameSize"`
	LowWaterBytes        uint64 `json:"lowWaterBytes"`
	HighWaterBytes       uint64 `json:"highWaterBytes"`
	MaximumBursts        int    `json:"maximumBursts"`
	ClientProbeMarker    byte   `json:"clientProbeMarker"`
	ClientBurstMarker    byte   `json:"clientBurstMarker"`
	ClientFinishedMarker byte   `json:"clientFinishedMarker"`
	ServerProbeMarker    byte   `json:"serverProbeMarker"`
	ServerBurstMarker    byte   `json:"serverBurstMarker"`
	ServerFinishedMarker byte   `json:"serverFinishedMarker"`
	ServerTerminalMarker byte   `json:"serverTerminalMarker"`
	CanceledSendMarker   byte   `json:"canceledSendMarker"`
	CancellationBarrier  byte   `json:"cancellationBarrier"`
	RemoteCloseMarker    byte   `json:"remoteCloseMarker"`
	TerminalFrameBytes   int    `json:"terminalFrameBytes"`
}

type observation struct {
	ChannelLabel             string   `json:"channelLabel"`
	ChannelProtocol          string   `json:"channelProtocol"`
	Ordered                  bool     `json:"ordered"`
	Reliable                 bool     `json:"reliable"`
	Negotiated               bool     `json:"negotiated"`
	SCTPMaxMessageSize       uint32   `json:"sctpMaxMessageSize"`
	ClientProbeReceived      bool     `json:"clientProbeReceived"`
	ClientBurstMessages      int      `json:"clientBurstMessages"`
	ServerProbeSent          bool     `json:"serverProbeSent"`
	ServerBurstMessages      int      `json:"serverBurstMessages"`
	ServerBufferPeak         uint64   `json:"serverBufferPeak"`
	TerminalAcknowledged     bool     `json:"terminalAcknowledged"`
	SendWaitObserved         bool     `json:"sendWaitObserved"`
	SendCanceled             bool     `json:"sendCanceled"`
	SendError                string   `json:"sendError"`
	SendErrorCanceled        bool     `json:"sendErrorCanceled"`
	SendErrorRemoteClosed    bool     `json:"sendErrorRemoteClosed"`
	ChannelDone              bool     `json:"channelDone"`
	ChannelStateClosed       bool     `json:"channelStateClosed"`
	ChannelError             string   `json:"channelError"`
	ChannelErrorRemoteClosed bool     `json:"channelErrorRemoteClosed"`
	ChannelCreated           bool     `json:"channelCreated"`
	ChannelOpened            bool     `json:"channelOpened"`
	ChannelStateObserved     bool     `json:"channelStateObserved"`
	InvalidChannelRejected   bool     `json:"invalidChannelRejected"`
	InvalidChannelError      string   `json:"invalidChannelError"`
	InvalidChannelErrorTyped bool     `json:"invalidChannelErrorTyped"`
	RawChannelState          string   `json:"rawChannelState"`
	RawChannelStateClosed    bool     `json:"rawChannelStateClosed"`
	PhysicalCloseSettled     bool     `json:"physicalCloseSettled"`
	PeerCloseSettled         bool     `json:"peerCloseSettled"`
	Events                   []string `json:"events"`
	Errors                   []string `json:"errors"`
}

type actionResponse struct {
	Action string `json:"action"`
}

type interopServer struct {
	peer    *pion.PeerConnection
	config  publicConfig
	handler http.Handler

	mu          sync.Mutex
	offerMu     sync.Mutex
	channelOnce sync.Once
	result      observation
	done        chan struct{}
	doneOnce    sync.Once
	actions     chan string
}

func main() {
	server, err := newInteropServer()
	if err != nil {
		panic(err)
	}
	defer server.peer.Close()
	address := os.Getenv("WINDSHARE_D1_CHROMIUM_ADDR")
	if address == "" {
		address = defaultAddress
	}
	fmt.Printf("WindShare D1 Chromium interop listening on http://%s\n", address)
	httpServer := &http.Server{
		Addr:              address,
		Handler:           server.handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		panic(err)
	}
}

func newInteropServer() (*interopServer, error) {
	scenario := os.Getenv(scenarioEnvName)
	if scenario == "" {
		scenario = scenarioHappy
	}
	switch scenario {
	case scenarioHappy, scenarioCancellation, scenarioRemoteClose, scenarioMalformed:
	default:
		return nil, fmt.Errorf("unknown Chromium interoperability scenario %q", scenario)
	}

	fixtureData, err := os.ReadFile("transport/webrtc/testdata/terminal-control.json")
	if err != nil {
		return nil, fmt.Errorf("read terminal-control fixture: %w", err)
	}
	var fixture terminalFixture
	if err := json.Unmarshal(fixtureData, &fixture); err != nil {
		return nil, fmt.Errorf("decode terminal-control fixture: %w", err)
	}
	peer, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("create Pion peer: %w", err)
	}
	server := &interopServer{
		peer: peer,
		config: publicConfig{
			Scenario:             scenario,
			ChannelLabel:         windwebrtc.ChannelLabel,
			ChannelProtocol:      windwebrtc.ChannelProtocol,
			InvalidProtocol:      invalidProtocol,
			TerminalIntent:       fixture.TerminalIntent,
			TerminalAck:          fixture.TerminalAck,
			MaxFrameSize:         session.MaxFrameSize,
			LowWaterBytes:        lowWaterBytes,
			HighWaterBytes:       highWaterBytes,
			MaximumBursts:        maximumBursts,
			ClientProbeMarker:    clientProbeMarker,
			ClientBurstMarker:    clientBurstMarker,
			ClientFinishedMarker: clientFinishedMarker,
			ServerProbeMarker:    serverProbeMarker,
			ServerBurstMarker:    serverBurstMarker,
			ServerFinishedMarker: serverFinishedMarker,
			ServerTerminalMarker: serverTerminalMarker,
			CanceledSendMarker:   canceledSendMarker,
			CancellationBarrier:  cancellationBarrier,
			RemoteCloseMarker:    remoteCloseMarker,
			TerminalFrameBytes:   terminalFrameBytes,
		},
		result:  observation{Events: []string{}, Errors: []string{}},
		done:    make(chan struct{}),
		actions: make(chan string, 1),
	}
	peer.OnDataChannel(server.onDataChannel)
	peer.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		server.event("peer-connection-" + state.String())
		if state == pion.PeerConnectionStateFailed {
			server.fail("Pion peer connection entered failed state")
		}
	})

	web, err := fs.Sub(assets, "web")
	if err != nil {
		_ = peer.Close()
		return nil, fmt.Errorf("open browser assets: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /config", server.handleConfig)
	mux.HandleFunc("POST /offer", server.handleOffer)
	mux.HandleFunc("GET /action", server.handleAction)
	mux.HandleFunc("GET /result", server.handleResult)
	mux.Handle("/", http.FileServer(http.FS(web)))
	server.handler = mux
	return server, nil
}

func (s *interopServer) handleAction(w http.ResponseWriter, request *http.Request) {
	select {
	case action := <-s.actions:
		writeJSON(w, http.StatusOK, actionResponse{Action: action})
	case <-s.done:
		http.Error(w, "scenario completed before a browser action was requested", http.StatusConflict)
	case <-request.Context().Done():
	}
}

func (s *interopServer) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.config)
}

func (s *interopServer) handleOffer(w http.ResponseWriter, request *http.Request) {
	s.offerMu.Lock()
	defer s.offerMu.Unlock()
	request.Body = http.MaxBytesReader(w, request.Body, 1024*1024)
	var offer pion.SessionDescription
	if err := json.NewDecoder(request.Body).Decode(&offer); err != nil {
		http.Error(w, "decode browser offer: "+err.Error(), http.StatusBadRequest)
		return
	}
	if offer.Type != pion.SDPTypeOffer {
		http.Error(w, "browser payload is not an SDP offer", http.StatusBadRequest)
		return
	}
	if err := s.peer.SetRemoteDescription(offer); err != nil {
		http.Error(w, "apply browser offer: "+err.Error(), http.StatusBadRequest)
		return
	}
	answer, err := s.peer.CreateAnswer(nil)
	if err != nil {
		http.Error(w, "create Pion answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	gathered := pion.GatheringCompletePromise(s.peer)
	if err := s.peer.SetLocalDescription(answer); err != nil {
		http.Error(w, "apply Pion answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), operationLimit)
	defer cancel()
	select {
	case <-gathered:
	case <-ctx.Done():
		http.Error(w, "Pion ICE gathering timed out", http.StatusGatewayTimeout)
		return
	}
	writeJSON(w, http.StatusOK, s.peer.LocalDescription())
}

func (s *interopServer) handleResult(w http.ResponseWriter, request *http.Request) {
	select {
	case <-s.done:
		s.mu.Lock()
		result := s.result
		result.Events = append([]string{}, result.Events...)
		result.Errors = append([]string{}, result.Errors...)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, result)
	case <-request.Context().Done():
	}
}

func (s *interopServer) onDataChannel(raw *pion.DataChannel) {
	accepted := false
	s.channelOnce.Do(func() {
		accepted = true
		s.mu.Lock()
		s.result.ChannelLabel = raw.Label()
		s.result.ChannelProtocol = raw.Protocol()
		s.result.Ordered = raw.Ordered()
		s.result.Reliable = raw.MaxPacketLifeTime() == nil && raw.MaxRetransmits() == nil
		s.result.Negotiated = raw.Negotiated()
		s.result.Events = append(s.result.Events, "adapter-construction-started")
		s.mu.Unlock()

		channel, err := windwebrtc.NewChannel(raw)
		if s.config.Scenario == scenarioMalformed {
			if err == nil {
				s.mu.Lock()
				s.result.ChannelCreated = true
				s.mu.Unlock()
				go s.settleUnexpectedMalformedAcceptance(channel)
				return
			}
			s.recordMalformedRejection(raw, err)
			return
		}
		if err != nil {
			s.fail("construct production Channel: " + err.Error())
			return
		}
		s.mu.Lock()
		s.result.ChannelCreated = true
		s.mu.Unlock()
		go s.runChannel(channel, raw)
	})
	if !accepted {
		s.fail("peer created more than one DataChannel")
	}
}

func (s *interopServer) runChannel(channel *windwebrtc.Channel, raw *pion.DataChannel) {
	timer := time.NewTimer(operationLimit)
	defer timer.Stop()
	select {
	case <-channel.Opened():
		s.event("channel-open")
		s.mu.Lock()
		s.result.ChannelOpened = true
		s.result.SCTPMaxMessageSize = raw.Transport().GetCapabilities().MaxMessageSize
		s.mu.Unlock()
	case <-channel.Done():
		s.fail("production Channel closed before opening: " + errorText(channel.Err()))
		return
	case <-timer.C:
		s.fail("production Channel did not open")
		return
	}

	for frame := range channel.Recv() {
		switch {
		case validPattern(frame, clientProbeMarker, session.MaxFrameSize):
			s.mu.Lock()
			s.result.ClientProbeReceived = true
			s.mu.Unlock()
		case len(frame) == session.MaxFrameSize && frame[0] == clientBurstMarker:
			s.mu.Lock()
			s.result.ClientBurstMessages++
			s.mu.Unlock()
		case len(frame) == 1 && frame[0] == clientFinishedMarker:
			s.runOutboundScenario(channel, raw)
			return
		default:
			s.fail(fmt.Sprintf("unexpected browser frame: len=%d marker=0x%02x", len(frame), firstByte(frame)))
			_ = channel.Close()
			return
		}
	}
	s.fail("production Channel Recv closed before the browser finished its burst")
}

func (s *interopServer) event(name string) {
	s.mu.Lock()
	s.result.Events = append(s.result.Events, name)
	s.mu.Unlock()
}

func (s *interopServer) fail(message string) {
	s.mu.Lock()
	s.result.Errors = append(s.result.Errors, message)
	s.mu.Unlock()
	s.complete()
}

func (s *interopServer) complete() {
	s.doneOnce.Do(func() { close(s.done) })
}

func patternedFrame(marker byte, size int) session.Frame {
	frame := make(session.Frame, size)
	if size == 0 {
		return frame
	}
	frame[0] = marker
	for index := 1; index < len(frame); index++ {
		frame[index] = byte((index*31 + 17) % 251)
	}
	return frame
}

func validPattern(frame []byte, marker byte, size int) bool {
	if len(frame) != size || size == 0 || frame[0] != marker {
		return false
	}
	for index := 1; index < len(frame); index++ {
		if frame[index] != byte((index*31+17)%251) {
			return false
		}
	}
	return true
}

func firstByte(frame []byte) byte {
	if len(frame) == 0 {
		return 0
	}
	return frame[0]
}

func errorText(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
