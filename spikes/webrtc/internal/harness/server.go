package harness

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/protocol"
)

//go:embed web/index.html web/spike.js
var webAssets embed.FS

var maxMessageSizePattern = regexp.MustCompile(`(?m)^a=max-message-size:(\d+)\r?$`)

type Harness struct {
	peer    *webrtc.PeerConnection
	handler http.Handler
	record  observationRecorder

	candidatesMu       sync.Mutex
	candidates         []json.RawMessage
	candidateGathering bool

	offerMu sync.Mutex

	dataMu          sync.Mutex
	clientBurstSeen int

	bufferedAmountLow chan struct{}
	serverBurstOnce   sync.Once
	terminalOnce      sync.Once
	closeOnce         sync.Once
}

func New() (*Harness, error) {
	peer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("create Pion peer connection: %w", err)
	}
	h := &Harness{
		peer:              peer,
		record:            newObservationRecorder(),
		bufferedAmountLow: make(chan struct{}, 8),
	}
	peer.OnICECandidate(h.onICECandidate)
	peer.OnDataChannel(h.onDataChannel)
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		h.record.event("peer-connection-" + state.String())
		if state == webrtc.PeerConnectionStateFailed {
			h.record.fail("Pion peer connection entered failed state")
		}
	})

	assets, err := fs.Sub(webAssets, "web")
	if err != nil {
		_ = peer.Close()
		return nil, fmt.Errorf("open embedded browser assets: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.handleHealth)
	mux.HandleFunc("GET /config", h.handleConfig)
	mux.HandleFunc("POST /signal", h.handleSignal)
	mux.HandleFunc("GET /signals", h.handleCandidates)
	mux.HandleFunc("GET /result", h.handleResult)
	mux.Handle("/", http.FileServer(http.FS(assets)))
	h.handler = mux
	return h, nil
}

func (h *Harness) Handler() http.Handler { return h.handler }

func (h *Harness) Close() error {
	if err := h.peer.Close(); err != nil {
		return fmt.Errorf("close Pion peer connection: %w", err)
	}
	return nil
}

func (h *Harness) Observation() Observation { return h.record.snapshot() }

func (h *Harness) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func (h *Harness) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, publicConfig())
}

func (h *Harness) handleResult(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.Observation())
}

func (h *Harness) handleSignal(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, protocol.MaxSignalingMessageBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read signal: "+err.Error(), http.StatusBadRequest)
		return
	}
	message, err := protocol.Decode(body)
	if err != nil {
		http.Error(w, "decode WindShare signal: "+err.Error(), http.StatusBadRequest)
		return
	}
	signalMessage, ok := message.(*protocol.Signal)
	if !ok {
		http.Error(w, "expected WindShare signal envelope", http.StatusBadRequest)
		return
	}
	if signalMessage.SessionID != spikeSessionID.String() {
		http.Error(w, "signal sessionId does not match the spike session", http.StatusBadRequest)
		return
	}

	switch signalMessage.Kind {
	case protocol.SignalKindOffer:
		h.handleOffer(w, signalMessage)
	case protocol.SignalKindCandidate:
		h.handleRemoteCandidate(w, signalMessage)
	default:
		http.Error(w, "unexpected signal kind "+signalMessage.Kind, http.StatusBadRequest)
	}
}

func (h *Harness) handleOffer(w http.ResponseWriter, signalMessage *protocol.Signal) {
	h.offerMu.Lock()
	defer h.offerMu.Unlock()

	var offer webrtc.SessionDescription
	if err := json.Unmarshal(signalMessage.Payload, &offer); err != nil {
		http.Error(w, "decode offer payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if offer.Type != webrtc.SDPTypeOffer {
		http.Error(w, "offer signal payload is not an SDP offer", http.StatusBadRequest)
		return
	}
	if err := h.peer.SetRemoteDescription(offer); err != nil {
		http.Error(w, "apply browser offer: "+err.Error(), http.StatusBadRequest)
		return
	}
	h.record.update(func(value *Observation) {
		value.OfferSignals++
		value.RemoteSDPMaxMessageSize = extractMaxMessageSize(offer.SDP)
	})
	answer, err := h.peer.CreateAnswer(nil)
	if err != nil {
		http.Error(w, "create Pion answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.peer.SetLocalDescription(answer); err != nil {
		http.Error(w, "apply Pion answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	local := h.peer.LocalDescription()
	if local == nil {
		http.Error(w, "Pion did not expose a local answer", http.StatusInternalServerError)
		return
	}
	payload, err := json.Marshal(local)
	if err != nil {
		http.Error(w, "encode Pion answer payload: "+err.Error(), http.StatusInternalServerError)
		return
	}
	encoded, err := protocol.Encode(protocol.NewSignal(spikeSessionID.String(), protocol.SignalKindAnswer, payload))
	if err != nil {
		http.Error(w, "encode WindShare answer signal: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.record.update(func(value *Observation) { value.AnswerSignals++ })
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func (h *Harness) handleRemoteCandidate(w http.ResponseWriter, signalMessage *protocol.Signal) {
	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal(signalMessage.Payload, &candidate); err != nil {
		http.Error(w, "decode browser ICE candidate: "+err.Error(), http.StatusBadRequest)
		return
	}
	if candidate.Candidate == "" {
		http.Error(w, "browser ICE candidate is empty", http.StatusBadRequest)
		return
	}
	if err := h.peer.AddICECandidate(candidate); err != nil {
		http.Error(w, "apply browser ICE candidate: "+err.Error(), http.StatusBadRequest)
		return
	}
	h.record.update(func(value *Observation) { value.BrowserCandidateSignals++ })
	encoded, err := protocol.Encode(protocol.NewSignal(spikeSessionID.String(), protocol.SignalKindCandidate, signalMessage.Payload))
	if err != nil {
		http.Error(w, "re-encode browser candidate signal: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func (h *Harness) onICECandidate(candidate *webrtc.ICECandidate) {
	if candidate == nil {
		h.candidatesMu.Lock()
		h.candidateGathering = true
		h.candidatesMu.Unlock()
		return
	}
	payload, err := json.Marshal(candidate.ToJSON())
	if err != nil {
		h.record.fail("encode Pion ICE candidate payload: " + err.Error())
		return
	}
	encoded, err := protocol.Encode(protocol.NewSignal(spikeSessionID.String(), protocol.SignalKindCandidate, payload))
	if err != nil {
		h.record.fail("encode Pion ICE candidate signal: " + err.Error())
		return
	}
	h.candidatesMu.Lock()
	h.candidates = append(h.candidates, json.RawMessage(encoded))
	h.candidatesMu.Unlock()
	h.record.update(func(value *Observation) { value.PionCandidateSignals++ })
}

func (h *Harness) handleCandidates(w http.ResponseWriter, r *http.Request) {
	after := 0
	if raw := r.URL.Query().Get("after"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			http.Error(w, "after must be a non-negative integer", http.StatusBadRequest)
			return
		}
		after = parsed
	}
	h.candidatesMu.Lock()
	if after > len(h.candidates) {
		h.candidatesMu.Unlock()
		http.Error(w, "after exceeds the candidate queue", http.StatusBadRequest)
		return
	}
	signals := append([]json.RawMessage(nil), h.candidates[after:]...)
	next := len(h.candidates)
	complete := h.candidateGathering
	h.candidatesMu.Unlock()
	writeJSON(w, http.StatusOK, struct {
		Signals  []json.RawMessage `json:"signals"`
		Next     int               `json:"next"`
		Complete bool              `json:"complete"`
	}{Signals: signals, Next: next, Complete: complete})
}

func (h *Harness) onDataChannel(channel *webrtc.DataChannel) {
	reliable := channel.MaxPacketLifeTime() == nil && channel.MaxRetransmits() == nil
	h.record.update(func(value *Observation) {
		value.ChannelLabel = channel.Label()
		value.ChannelProtocol = channel.Protocol()
		value.Ordered = channel.Ordered()
		value.Reliable = reliable
		value.Negotiated = channel.Negotiated()
	})
	if channel.Label() != ChannelLabel || channel.Protocol() != ChannelProtocol || !channel.Ordered() || !reliable || channel.Negotiated() {
		h.record.fail("browser DataChannel parameters do not match the ordered/reliable WindShare contract")
	}
	channel.SetBufferedAmountLowThreshold(lowWaterMarkBytes)
	channel.OnBufferedAmountLow(func() {
		select {
		case h.bufferedAmountLow <- struct{}{}:
		default:
		}
	})
	channel.OnError(func(err error) {
		h.record.fail("Pion DataChannel error: " + err.Error())
	})
	channel.OnClose(func() {
		h.record.event("channel-closed")
		h.record.update(func(value *Observation) { value.ChannelClosed = true })
	})
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		if message.IsString {
			h.handleControl(channel, message.Data)
			return
		}
		h.handleBinary(channel, message.Data)
	})
	channel.OnOpen(func() {
		h.record.event("channel-open")
		if err := sendControl(channel, map[string]any{"type": "server-ready"}); err != nil {
			h.record.fail("send server-ready: " + err.Error())
		}
	})
}

type clientControl struct {
	Type     string `json:"type"`
	Count    int    `json:"count"`
	Peak     uint64 `json:"peak"`
	LowEvent bool   `json:"lowEvent"`
}

func (h *Harness) handleControl(channel *webrtc.DataChannel, data []byte) {
	var control clientControl
	if err := json.Unmarshal(data, &control); err != nil {
		h.record.fail("decode browser control message: " + err.Error())
		return
	}
	switch control.Type {
	case "client-burst-complete":
		h.dataMu.Lock()
		seen := h.clientBurstSeen
		h.dataMu.Unlock()
		if seen != control.Count || control.Peak < highWaterMarkBytes || !control.LowEvent {
			h.record.fail(fmt.Sprintf("browser backpressure report is inconsistent: seen=%d reported=%d peak=%d low=%t", seen, control.Count, control.Peak, control.LowEvent))
		}
		h.record.update(func(value *Observation) {
			value.BrowserBurstMessages = seen
			value.BrowserBackpressurePeak = control.Peak
			value.BrowserBufferedAmountLow = control.LowEvent
		})
		if err := sendControl(channel, map[string]any{"type": "client-burst-accepted", "count": seen}); err != nil {
			h.record.fail("acknowledge browser burst: " + err.Error())
		}
	case "start-server-burst":
		h.serverBurstOnce.Do(func() { go h.runServerBurst(channel) })
	case "server-terminal-received":
		h.closeOnce.Do(func() {
			h.record.event("server-terminal-acknowledged")
			if err := channel.Close(); err != nil {
				h.record.fail("close DataChannel after terminal acknowledgement: " + err.Error())
				return
			}
			h.record.event("channel-close-requested")
		})
	default:
		h.record.fail("unexpected browser control message " + control.Type)
	}
}

func (h *Harness) handleBinary(channel *webrtc.DataChannel, data []byte) {
	if validPattern(data, clientProbeMarker, session.MaxFrameSize) {
		h.record.update(func(value *Observation) { value.BrowserProbeReceived = true })
		if err := sendControl(channel, map[string]any{"type": "client-probe-accepted"}); err != nil {
			h.record.fail("acknowledge browser 64 KiB probe: " + err.Error())
		}
		return
	}
	if len(data) == session.MaxFrameSize && data[0] == clientBurstMarker {
		h.dataMu.Lock()
		h.clientBurstSeen++
		h.dataMu.Unlock()
		return
	}
	if validPattern(data, clientTerminalMarker, terminalFrameBytes) {
		h.record.event("client-terminal-received")
		h.record.update(func(value *Observation) { value.ClientTerminalReceived = true })
		h.terminalOnce.Do(func() { go h.sendTerminalAndClose(channel) })
		return
	}
	h.record.fail(fmt.Sprintf("unexpected browser binary message: len=%d marker=0x%02x", len(data), firstByte(data)))
}

func (h *Harness) runServerBurst(channel *webrtc.DataChannel) {
	for {
		select {
		case <-h.bufferedAmountLow:
		default:
			goto drained
		}
	}

drained:
	if err := channel.Send(patternedFrame(serverProbeMarker, session.MaxFrameSize)); err != nil {
		h.serverBurstFailure(channel, "send Pion 64 KiB probe: "+err.Error())
		return
	}
	h.record.update(func(value *Observation) { value.PionProbeSent = true })
	frame := patternedFrame(serverBurstMarker, session.MaxFrameSize)
	count := 0
	peak := channel.BufferedAmount()
	for peak < highWaterMarkBytes && count < maxBurstMessages {
		if err := channel.Send(frame); err != nil {
			h.serverBurstFailure(channel, "send Pion backpressure frame: "+err.Error())
			return
		}
		count++
		peak = max(peak, channel.BufferedAmount())
	}
	if peak < highWaterMarkBytes {
		h.serverBurstFailure(channel, fmt.Sprintf("Pion bufferedAmount peaked at %d before the %d-message safety bound", peak, maxBurstMessages))
		return
	}

	timer := time.NewTimer(backpressureTimeout)
	defer timer.Stop()
	lowEvent := waitForBufferedAmountLow(
		h.bufferedAmountLow,
		timer.C,
		channel.BufferedAmount,
		lowWaterMarkBytes,
	)
	if !lowEvent {
		h.serverBurstFailure(channel, "Pion bufferedAmount did not cross the low threshold before timeout")
		return
	}
	h.record.update(func(value *Observation) {
		value.PionBurstMessages = count
		value.PionBackpressurePeak = peak
		value.PionBufferedAmountLow = lowEvent
	})
	if err := sendControl(channel, map[string]any{
		"type":     "server-burst-complete",
		"count":    count,
		"peak":     peak,
		"lowEvent": lowEvent,
	}); err != nil {
		h.record.fail("report Pion backpressure result: " + err.Error())
	}
}

func waitForBufferedAmountLow(
	wakes <-chan struct{},
	deadline <-chan time.Time,
	bufferedAmount func() uint64,
	lowWaterMark uint64,
) bool {
	for {
		select {
		case <-wakes:
			// A callback is only a scheduling hint: an older crossing may still be
			// queued, so the authoritative amount must satisfy this burst's threshold.
			if bufferedAmount() <= lowWaterMark {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func (h *Harness) serverBurstFailure(channel *webrtc.DataChannel, message string) {
	h.record.fail(message)
	_ = sendControl(channel, map[string]any{"type": "server-burst-failed", "message": message})
}

func (h *Harness) sendTerminalAndClose(channel *webrtc.DataChannel) {
	if err := channel.Send(patternedFrame(serverTerminalMarker, terminalFrameBytes)); err != nil {
		h.record.fail("send server terminal frame: " + err.Error())
		return
	}
	h.record.event("server-terminal-sent")
	h.record.update(func(value *Observation) { value.ServerTerminalSent = true })
}

func sendControl(channel *webrtc.DataChannel, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode control message: %w", err)
	}
	if err := channel.SendText(string(payload)); err != nil {
		return fmt.Errorf("send control message: %w", err)
	}
	return nil
}

func extractMaxMessageSize(sdp string) uint64 {
	match := maxMessageSizePattern.FindStringSubmatch(sdp)
	if len(match) != 2 {
		return 0
	}
	value, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func firstByte(data []byte) byte {
	if len(data) == 0 {
		return 0
	}
	return data[0]
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return
	}
}
