package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/framechannel"
	windwebrtc "github.com/windshare/windshare/transport/webrtc"
)

const (
	defaultAddress  = "127.0.0.1:17846"
	scenarioEnvName = "WINDSHARE_D1_BROWSER_SCENARIO"
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
	Scenario             string                  `json:"scenario"`
	ChannelLabel         string                  `json:"channelLabel"`
	ChannelProtocol      string                  `json:"channelProtocol"`
	InvalidProtocol      string                  `json:"invalidProtocol"`
	TerminalIntent       string                  `json:"terminalIntent"`
	TerminalAck          string                  `json:"terminalAck"`
	MaxFrameSize         int                     `json:"maxFrameSize"`
	LowWaterBytes        uint64                  `json:"lowWaterBytes"`
	HighWaterBytes       uint64                  `json:"highWaterBytes"`
	MaximumBursts        int                     `json:"maximumBursts"`
	ClientProbeMarker    byte                    `json:"clientProbeMarker"`
	ClientBurstMarker    byte                    `json:"clientBurstMarker"`
	ClientFinishedMarker byte                    `json:"clientFinishedMarker"`
	ServerProbeMarker    byte                    `json:"serverProbeMarker"`
	ServerBurstMarker    byte                    `json:"serverBurstMarker"`
	ServerFinishedMarker byte                    `json:"serverFinishedMarker"`
	ServerTerminalMarker byte                    `json:"serverTerminalMarker"`
	CanceledSendMarker   byte                    `json:"canceledSendMarker"`
	CancellationBarrier  byte                    `json:"cancellationBarrier"`
	RemoteCloseMarker    byte                    `json:"remoteCloseMarker"`
	TerminalFrameBytes   int                     `json:"terminalFrameBytes"`
	TopologyLock         *serializedTopologyLock `json:"topologyLock,omitempty"`
}

type observation struct {
	AttemptID                string                    `json:"attemptId,omitempty"`
	SelectedPair             *pionSelectedPairEvidence `json:"selectedPair,omitempty"`
	TopologyProfileSHA256    string                    `json:"topologyProfileSha256,omitempty"`
	TopologyResolutionSHA256 string                    `json:"topologyResolutionSha256,omitempty"`
	ChannelLabel             string                    `json:"channelLabel"`
	ChannelProtocol          string                    `json:"channelProtocol"`
	Ordered                  bool                      `json:"ordered"`
	Reliable                 bool                      `json:"reliable"`
	Negotiated               bool                      `json:"negotiated"`
	SCTPMaxMessageSize       uint32                    `json:"sctpMaxMessageSize"`
	ClientProbeReceived      bool                      `json:"clientProbeReceived"`
	ClientBurstMessages      int                       `json:"clientBurstMessages"`
	ServerProbeSent          bool                      `json:"serverProbeSent"`
	ServerBurstMessages      int                       `json:"serverBurstMessages"`
	ServerBufferPeak         uint64                    `json:"serverBufferPeak"`
	TerminalAcknowledged     bool                      `json:"terminalAcknowledged"`
	SendWaitObserved         bool                      `json:"sendWaitObserved"`
	SendCanceled             bool                      `json:"sendCanceled"`
	SendError                string                    `json:"sendError"`
	SendErrorCanceled        bool                      `json:"sendErrorCanceled"`
	SendErrorRemoteClosed    bool                      `json:"sendErrorRemoteClosed"`
	ChannelDone              bool                      `json:"channelDone"`
	ChannelStateClosed       bool                      `json:"channelStateClosed"`
	ChannelError             string                    `json:"channelError"`
	ChannelErrorRemoteClosed bool                      `json:"channelErrorRemoteClosed"`
	ChannelCreated           bool                      `json:"channelCreated"`
	ChannelOpened            bool                      `json:"channelOpened"`
	ChannelStateObserved     bool                      `json:"channelStateObserved"`
	InvalidChannelRejected   bool                      `json:"invalidChannelRejected"`
	InvalidChannelError      string                    `json:"invalidChannelError"`
	InvalidChannelErrorTyped bool                      `json:"invalidChannelErrorTyped"`
	RawChannelState          string                    `json:"rawChannelState"`
	RawChannelStateClosed    bool                      `json:"rawChannelStateClosed"`
	PhysicalCloseSettled     bool                      `json:"physicalCloseSettled"`
	PeerCloseSettled         bool                      `json:"peerCloseSettled"`
	Events                   []string                  `json:"events"`
	Errors                   []string                  `json:"errors"`
}

type actionResponse struct {
	Action string `json:"action"`
}

type interopServer struct {
	peer     *pion.PeerConnection
	config   publicConfig
	handler  http.Handler
	topology *topologyRuntime

	mu          sync.Mutex
	offerMu     sync.Mutex
	channelOnce sync.Once
	result      observation
	done        chan struct{}
	doneOnce    sync.Once
	actions     chan string
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "self-check" {
		fmt.Println(`{"schemaVersion":1,"component":"pion-browser-interop-server","outcome":"ready"}`)
		return
	}
	if len(os.Args) != 1 {
		panic("Pion browser interop server accepts only the self-check command")
	}
	topologyContext, cancelTopology := context.WithTimeout(context.Background(), operationLimit)
	defer cancelTopology()
	topology, err := loadTopologyRuntime(topologyContext)
	if err != nil {
		panic(err)
	}
	server, err := newInteropServer(topology)
	if err != nil {
		panic(err)
	}
	defer server.closeCurrentPeer()
	address := os.Getenv("WINDSHARE_D1_BROWSER_ADDR")
	if address == "" {
		address = defaultAddress
	}
	fmt.Printf("WindShare D1 browser interop listening on http://%s\n", address)
	httpServer := &http.Server{
		Addr:              address,
		Handler:           server.handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		panic(err)
	}
}

// newLoopbackOnlyPeer confines ICE to loopback addresses. The interop suite
// is strictly localhost — the browser, the Vite server, and this helper all
// live on 127.0.0.1 — so non-loopback host candidates add nothing, and
// non-loopback bindings needlessly expose the helper to host interfaces and may
// trigger host-owned firewall consent UI. That UI and firewall state are outside
// the test verdict; limiting the helper to its real trust boundary also keeps
// normal execution independent of either. mDNS is disabled because its responder
// binds a wildcard UDP socket. Loopback-only sockets avoid that extra exposure.
// Compare benchmarkLoopbackAPI in transport/webrtc/performance_test.go.
func newLoopbackOnlyPeer() (*pion.PeerConnection, error) {
	var setting pion.SettingEngine
	setting.SetIncludeLoopbackCandidate(true)
	setting.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	setting.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return pion.NewAPI(pion.WithSettingEngine(setting)).NewPeerConnection(pion.Configuration{})
}

func (s *interopServer) newPeerConnection() (*pion.PeerConnection, error) {
	if s.topology == nil {
		return newLoopbackOnlyPeer()
	}
	return s.topology.newPeerConnection()
}

func newInteropServer(topology *topologyRuntime) (*interopServer, error) {
	scenario := os.Getenv(scenarioEnvName)
	if scenario == "" {
		scenario = scenarioHappy
	}
	switch scenario {
	case scenarioHappy, scenarioCancellation, scenarioRemoteClose, scenarioMalformed:
	default:
		return nil, fmt.Errorf("unknown browser interoperability scenario %q", scenario)
	}

	fixtureData, err := os.ReadFile("transport/webrtc/testdata/terminal-control.json")
	if err != nil {
		return nil, fmt.Errorf("read terminal-control fixture: %w", err)
	}
	var fixture terminalFixture
	if err := json.Unmarshal(fixtureData, &fixture); err != nil {
		return nil, fmt.Errorf("decode terminal-control fixture: %w", err)
	}
	server := &interopServer{
		topology: topology,
		config: publicConfig{
			Scenario:             scenario,
			ChannelLabel:         windwebrtc.ChannelLabel,
			ChannelProtocol:      windwebrtc.ChannelProtocol,
			InvalidProtocol:      invalidProtocol,
			TerminalIntent:       fixture.TerminalIntent,
			TerminalAck:          fixture.TerminalAck,
			MaxFrameSize:         framechannel.MaxFrameSize,
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
	}
	if topology != nil {
		publicLock := topology.public
		server.config.TopologyLock = &publicLock
	}
	if err := server.replacePeer(); err != nil {
		return nil, err
	}

	web, err := fs.Sub(assets, "web")
	if err != nil {
		server.closeCurrentPeer()
		return nil, fmt.Errorf("open browser assets: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /config", server.handleConfig)
	mux.HandleFunc("POST /offer", server.handleOffer)
	mux.HandleFunc("GET /action", server.handleAction)
	mux.HandleFunc("GET /result", server.handleResult)
	mux.HandleFunc("GET /snapshot", server.handleSnapshot)
	mux.Handle("/", http.FileServer(http.FS(web)))
	server.handler = mux
	return server, nil
}

func (s *interopServer) replacePeer() error {
	peer, err := s.newPeerConnection()
	if err != nil {
		return fmt.Errorf("create Pion peer: %w", err)
	}
	s.mu.Lock()
	previous := s.peer
	s.peer = peer
	s.channelOnce = sync.Once{}
	s.result = observation{Events: []string{}, Errors: []string{}}
	if s.topology != nil {
		s.result.TopologyProfileSHA256 = s.topology.profileSHA256
		s.result.TopologyResolutionSHA256 = s.topology.resolutionSHA256
	}
	s.done = make(chan struct{})
	s.doneOnce = sync.Once{}
	s.actions = make(chan string, 1)
	s.mu.Unlock()

	peer.OnDataChannel(func(raw *pion.DataChannel) {
		if !s.isCurrentPeer(peer) {
			_ = raw.Close()
			return
		}
		s.onDataChannel(peer, raw)
	})
	peer.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		if !s.isCurrentPeer(peer) {
			return
		}
		s.event("peer-connection-" + state.String())
		if state == pion.PeerConnectionStateFailed {
			s.fail("Pion peer connection entered failed state")
		}
	})
	if previous != nil {
		_ = previous.Close()
	}
	return nil
}

func (s *interopServer) preparePeerForOffer() error {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	select {
	case <-done:
		return s.replacePeer()
	default:
		return nil
	}
}

func (s *interopServer) isCurrentPeer(peer *pion.PeerConnection) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peer == peer
}

func (s *interopServer) currentPeer() *pion.PeerConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peer
}

func (s *interopServer) closeCurrentPeer() {
	if peer := s.currentPeer(); peer != nil {
		_ = peer.Close()
	}
}

func (s *interopServer) handleAction(w http.ResponseWriter, request *http.Request) {
	s.mu.Lock()
	actions := s.actions
	done := s.done
	s.mu.Unlock()
	select {
	case action := <-actions:
		writeJSON(w, http.StatusOK, actionResponse{Action: action})
	case <-done:
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
	if err := s.preparePeerForOffer(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	peer := s.currentPeer()
	if peer == nil {
		http.Error(w, "Pion peer is unavailable", http.StatusInternalServerError)
		return
	}
	request.Body = http.MaxBytesReader(w, request.Body, 1024*1024)
	offer, attemptID, err := s.decodeOffer(request)
	if err != nil {
		http.Error(w, "decode browser offer: "+err.Error(), http.StatusBadRequest)
		return
	}
	if offer.Type != pion.SDPTypeOffer {
		http.Error(w, "browser payload is not an SDP offer", http.StatusBadRequest)
		return
	}
	if attemptID != "" {
		s.mu.Lock()
		if s.result.AttemptID != "" {
			s.mu.Unlock()
			http.Error(w, "Pion peer already owns a native attempt", http.StatusConflict)
			return
		}
		s.result.AttemptID = attemptID
		s.result.Events = append(s.result.Events, "native-attempt-started")
		s.mu.Unlock()
	}
	if err := peer.SetRemoteDescription(offer); err != nil {
		s.fail("apply browser offer: " + err.Error())
		http.Error(w, "apply browser offer: "+err.Error(), http.StatusBadRequest)
		return
	}
	answer, err := peer.CreateAnswer(nil)
	if err != nil {
		s.fail("create Pion answer: " + err.Error())
		http.Error(w, "create Pion answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	gathered := pion.GatheringCompletePromise(peer)
	if err := peer.SetLocalDescription(answer); err != nil {
		s.fail("apply Pion answer: " + err.Error())
		http.Error(w, "apply Pion answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), operationLimit)
	defer cancel()
	select {
	case <-gathered:
	case <-ctx.Done():
		s.fail("Pion ICE gathering timed out")
		http.Error(w, "Pion ICE gathering timed out", http.StatusGatewayTimeout)
		return
	}
	writeJSON(w, http.StatusOK, peer.LocalDescription())
}

type topologyOfferRequest struct {
	AttemptID string                  `json:"attemptId"`
	Offer     pion.SessionDescription `json:"offer"`
}

func (s *interopServer) decodeOffer(request *http.Request) (pion.SessionDescription, string, error) {
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if s.topology == nil {
		var offer pion.SessionDescription
		if err := decoder.Decode(&offer); err != nil {
			return pion.SessionDescription{}, "", err
		}
		if err := requireJSONEnd(decoder); err != nil {
			return pion.SessionDescription{}, "", err
		}
		return offer, "", nil
	}
	var envelope topologyOfferRequest
	if err := decoder.Decode(&envelope); err != nil {
		return pion.SessionDescription{}, "", err
	}
	if err := requireJSONEnd(decoder); err != nil {
		return pion.SessionDescription{}, "", err
	}
	if !validAttemptID(envelope.AttemptID) {
		return pion.SessionDescription{}, "", fmt.Errorf("attemptId is not canonical nonzero 16-byte base64url")
	}
	return envelope.Offer, envelope.AttemptID, nil
}

func requireJSONEnd(decoder *json.Decoder) error {
	var trailing json.RawMessage
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("request contains more than one JSON value")
	}
	return err
}

func (s *interopServer) handleResult(w http.ResponseWriter, request *http.Request) {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	select {
	case <-done:
		writeJSON(w, http.StatusOK, s.resultSnapshot())
	case <-request.Context().Done():
	}
}

func (s *interopServer) handleSnapshot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.resultSnapshot())
}

func (s *interopServer) resultSnapshot() observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.result
	result.Events = append([]string{}, result.Events...)
	result.Errors = append([]string{}, result.Errors...)
	if result.SelectedPair != nil {
		pair := *result.SelectedPair
		result.SelectedPair = &pair
	}
	return result
}

func (s *interopServer) onDataChannel(peer *pion.PeerConnection, raw *pion.DataChannel) {
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
		go s.runChannel(peer, channel, raw)
	})
	if !accepted {
		s.fail("peer created more than one DataChannel")
	}
}

func (s *interopServer) runChannel(
	peer *pion.PeerConnection,
	channel *windwebrtc.Channel,
	raw *pion.DataChannel,
) {
	timer := time.NewTimer(operationLimit)
	defer timer.Stop()
	select {
	case <-channel.Opened():
		s.event("channel-open")
		s.mu.Lock()
		s.result.ChannelOpened = true
		s.result.SCTPMaxMessageSize = raw.Transport().GetCapabilities().MaxMessageSize
		s.mu.Unlock()
		if s.topology != nil {
			pair, err := s.topology.selectedPairEvidence(peer)
			if err != nil {
				s.fail("capture topology-bound Pion selected pair: " + err.Error())
				_ = channel.Close()
				return
			}
			s.mu.Lock()
			if s.result.AttemptID == "" {
				s.mu.Unlock()
				s.fail("Pion DataChannel opened without a correlated native attempt ID")
				_ = channel.Close()
				return
			}
			s.result.SelectedPair = &pair
			s.result.Events = append(s.result.Events, "selected-pair-captured")
			s.mu.Unlock()
		}
	case <-channel.Done():
		s.fail("production Channel closed before opening: " + errorText(channel.Err()))
		return
	case <-timer.C:
		s.fail("production Channel did not open")
		return
	}

	for frame := range channel.Recv() {
		switch {
		case validPattern(frame, clientProbeMarker, framechannel.MaxFrameSize):
			s.mu.Lock()
			s.result.ClientProbeReceived = true
			s.mu.Unlock()
		case len(frame) == framechannel.MaxFrameSize && frame[0] == clientBurstMarker:
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

func patternedFrame(marker byte, size int) framechannel.Frame {
	frame := make(framechannel.Frame, size)
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
