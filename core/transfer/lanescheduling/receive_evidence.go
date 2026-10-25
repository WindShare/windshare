package lanescheduling

import "time"

func (w *ReceiveWindow) Load(now time.Time) DeliveryLoad {
	load := DeliveryLoad{RemainingBytes: w.remaining, Evidence: DeliveryUnknown}
	for _, response := range w.responses {
		if response.remaining > 0 {
			load.PendingResponses++
		}
	}
	if w.rate <= 0 || w.lastReceipt.IsZero() {
		return load
	}
	load.Evidence, load.BytesPerSecond = DeliveryRecent, w.rate
	if now.Sub(w.lastReceipt) > max(EvidenceLifetime, 2*w.response) {
		load.Evidence = DeliveryStale
	}
	// Whole-response throughput already includes response delay. A separate
	// startup cost belongs only to inter-fragment delivery samples.
	if w.streamingRate {
		load.Startup = w.response
	}
	return load
}

func (w *ReceiveWindow) EstimateCompletion(bytes uint64, now time.Time) DeliveryEstimate {
	return EstimateCompletion(w.Load(now), bytes)
}

func (w *ReceiveWindow) EstimateQueue(now time.Time) time.Duration {
	return EstimateQueue(w.Load(now))
}

func (p *ReceivePermit) Progress(now time.Time) DeliveryProgress {
	load := p.window.Load(now)
	load.RemainingBytes, load.PendingResponses = 0, 0
	// Newer reservations must not postpone an older attempt's prediction.
	for _, earlier := range p.window.responses {
		if earlier == p {
			break
		}
		load.RemainingBytes += earlier.remaining
		if earlier.remaining > 0 {
			load.PendingResponses++
		}
	}
	elapsed := max(time.Duration(0), now.Sub(p.started))
	if p.first {
		load.Startup = max(0, load.Startup-elapsed)
	} else {
		load.Startup = 0
	}
	progressAt := p.lastReceipt
	if progressAt.IsZero() {
		progressAt = p.started
	}
	return DeliveryProgress{
		RemainingBytes: p.remaining, Elapsed: elapsed, Silent: max(0, now.Sub(progressAt)),
		Remaining: EstimateCompletion(load, p.remaining),
	}
}
