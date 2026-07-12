package harness

import "sync"

type Observation struct {
	OfferSignals             int      `json:"offerSignals"`
	AnswerSignals            int      `json:"answerSignals"`
	BrowserCandidateSignals  int      `json:"browserCandidateSignals"`
	PionCandidateSignals     int      `json:"pionCandidateSignals"`
	ChannelLabel             string   `json:"channelLabel"`
	ChannelProtocol          string   `json:"channelProtocol"`
	Ordered                  bool     `json:"ordered"`
	Reliable                 bool     `json:"reliable"`
	Negotiated               bool     `json:"negotiated"`
	BrowserProbeReceived     bool     `json:"browserProbeReceived"`
	BrowserBurstMessages     int      `json:"browserBurstMessages"`
	BrowserBackpressurePeak  uint64   `json:"browserBackpressurePeak"`
	BrowserBufferedAmountLow bool     `json:"browserBufferedAmountLow"`
	PionProbeSent            bool     `json:"pionProbeSent"`
	PionBurstMessages        int      `json:"pionBurstMessages"`
	PionBackpressurePeak     uint64   `json:"pionBackpressurePeak"`
	PionBufferedAmountLow    bool     `json:"pionBufferedAmountLow"`
	ClientTerminalReceived   bool     `json:"clientTerminalReceived"`
	ServerTerminalSent       bool     `json:"serverTerminalSent"`
	ChannelClosed            bool     `json:"channelClosed"`
	RemoteSDPMaxMessageSize  uint64   `json:"remoteSdpMaxMessageSize"`
	Events                   []string `json:"events"`
	Errors                   []string `json:"errors"`
}

type observationRecorder struct {
	mu    sync.Mutex
	value Observation
}

func newObservationRecorder() observationRecorder {
	return observationRecorder{value: Observation{Events: []string{}, Errors: []string{}}}
}

func (r *observationRecorder) update(update func(*Observation)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	update(&r.value)
}

func (r *observationRecorder) event(name string) {
	r.update(func(value *Observation) {
		value.Events = append(value.Events, name)
	})
}

func (r *observationRecorder) fail(message string) {
	r.update(func(value *Observation) {
		value.Errors = append(value.Errors, message)
	})
}

func (r *observationRecorder) snapshot() Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := r.value
	result.Events = append([]string{}, r.value.Events...)
	result.Errors = append([]string{}, r.value.Errors...)
	return result
}

func eventPrecedes(events []string, before, after string) bool {
	beforeIndex := -1
	for index, event := range events {
		if event == before && beforeIndex == -1 {
			beforeIndex = index
		}
		if event == after {
			return beforeIndex >= 0 && beforeIndex < index
		}
	}
	return false
}
