package lanescheduling

import (
	"slices"
	"time"
)

const (
	receiveAhead        = 250 * time.Millisecond
	receiveSampleWindow = 250 * time.Millisecond
	receiveSampleWeight = 0.25
)

// ReceiveWindow owns unfinished responses, independently of authenticated
// plaintext or output retention. Its caller serializes access with lane state.
type ReceiveWindow struct {
	responses     []*ReceivePermit
	streamingRate bool
	active        uint32
	receiving     uint32
	remaining     uint64
	rate          float64
	response      time.Duration
	sampleSince   time.Time
	sampleBytes   uint64
	lastReceipt   time.Time
}

type ReceivePermit struct {
	window      *ReceiveWindow
	started     time.Time
	lastReceipt time.Time
	remaining   uint64
	first       bool
	receiving   bool
	closed      bool
}

func (w *ReceiveWindow) UnfinishedBytes() uint64 { return w.remaining }

func (w *ReceiveWindow) Available() bool {
	ahead := w.rate * (w.response + receiveAhead).Seconds()
	return w.active == 0 || (w.rate > 0 && float64(w.remaining) <= ahead)
}

func (w *ReceiveWindow) TryReserve(bytes uint64, now time.Time) *ReceivePermit {
	if bytes == 0 || !w.Available() {
		return nil
	}
	// Silent active lanes also need fresh receipt before refilling a pipeline.
	if w.Load(now).Evidence == DeliveryStale {
		w.rate = 0
		w.response = 0
		w.streamingRate = false
	}
	if w.active == 0 {
		w.sampleSince, w.sampleBytes = now, 0
	}
	w.active++
	w.remaining += bytes
	permit := &ReceivePermit{window: w, started: now, remaining: bytes, first: true}
	w.responses = append(w.responses, permit)
	return permit
}

// Receive updates every fragment's supply estimate and reports a blocked-to-ready
// transition. Waiters check and reserve under the same owner lock, so another
// broadcast is unnecessary while capacity stays available.
func (p *ReceivePermit) Receive(bytes uint64, now time.Time) bool {
	if p == nil || p.closed || bytes == 0 {
		return false
	}
	w := p.window
	wasAvailable := w.Available()
	received := min(bytes, p.remaining)
	p.remaining -= received
	if received > 0 {
		p.lastReceipt = now
	}
	w.remaining -= received
	w.sampleDelivery(p, bytes, now)
	if p.remaining > 0 && !p.receiving {
		p.receiving = true
		w.receiving++
	}
	if p.remaining == 0 && p.receiving {
		p.receiving = false
		w.receiving--
	}
	return !wasAvailable && w.Available()
}

func (w *ReceiveWindow) sampleDelivery(p *ReceivePermit, bytes uint64, now time.Time) {
	streaming := w.receiving > 0 || (!w.lastReceipt.IsZero() && now.Sub(w.lastReceipt) < w.response)
	w.lastReceipt = now
	if !streaming {
		w.sampleSince, w.sampleBytes = now, 0
	}
	if p.first {
		response := max(time.Millisecond, now.Sub(p.started))
		if w.response == 0 || response < w.response {
			w.response = response
		}
		p.first = false
	}
	retainStreamingRate := !streaming && w.streamingRate && p.remaining > 0
	w.streamingRate = streaming || retainStreamingRate
	w.sampleBytes += bytes
	elapsed := max(time.Millisecond, now.Sub(w.sampleSince))
	rate := float64(w.sampleBytes) / elapsed.Seconds()
	if !streaming {
		rate = float64(bytes) / max(time.Millisecond, now.Sub(p.started)).Seconds()
	}
	// A first fragment mixes response latency with supply. Wait for a second
	// fragment before replacing an established streaming-capacity observation.
	if !retainStreamingRate {
		if w.rate == 0 || rate < w.rate {
			w.rate = rate
		} else {
			w.rate += receiveSampleWeight * (rate - w.rate)
		}
	}
	if !streaming || elapsed >= receiveSampleWindow {
		w.sampleSince, w.sampleBytes = now, 0
	}
}

func (p *ReceivePermit) Complete(now time.Time) {
	if p != nil {
		p.Receive(p.remaining, now)
	}
}

func (p *ReceivePermit) Close() {
	if p == nil || p.closed {
		return
	}
	p.closed = true
	p.window.active--
	p.window.responses = slices.DeleteFunc(p.window.responses, func(other *ReceivePermit) bool { return other == p })
	p.window.remaining -= p.remaining
	if p.receiving {
		p.window.receiving--
	}
}
