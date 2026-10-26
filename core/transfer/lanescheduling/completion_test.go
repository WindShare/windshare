package lanescheduling

import (
	"testing"
	"time"
)

// Forecasts are native policy. Protocol interoperability does not require a
// browser receiver to use the same bootstrap timing or consumption estimate.
func TestDeliveryForecast(t *testing.T) {
	const block = uint64(1000)
	tests := []struct {
		name       string
		load       DeliveryLoad
		bytes      uint64
		queue      time.Duration
		completion DeliveryEstimate
	}{
		{
			name: "cold idle lane has no measured capacity",
			load: DeliveryLoad{Evidence: DeliveryUnknown}, bytes: block,
			completion: DeliveryEstimate{Evidence: DeliveryUnknown, Duration: InitialBlockTime},
		},
		{
			name: "cold queue counts outstanding responses",
			load: DeliveryLoad{RemainingBytes: 5000, PendingResponses: 2, Evidence: DeliveryUnknown}, bytes: block,
			queue: 2 * InitialBlockTime, completion: DeliveryEstimate{Evidence: DeliveryUnknown, Duration: 3 * InitialBlockTime},
		},
		{
			name: "receipt reduces remaining network work",
			load: DeliveryLoad{RemainingBytes: block, PendingResponses: 2, Evidence: DeliveryRecent,
				BytesPerSecond: 5000, Startup: 100 * time.Millisecond}, bytes: block,
			queue: 200 * time.Millisecond, completion: DeliveryEstimate{Evidence: DeliveryRecent, Duration: 400 * time.Millisecond},
		},
		{
			name: "response startup overlaps a short queue",
			load: DeliveryLoad{RemainingBytes: block, PendingResponses: 1, Evidence: DeliveryRecent,
				BytesPerSecond: 10000, Startup: 200 * time.Millisecond}, bytes: block,
			queue: 100 * time.Millisecond, completion: DeliveryEstimate{Evidence: DeliveryRecent, Duration: 300 * time.Millisecond},
		},
		{
			name: "queued transmission hides response startup",
			load: DeliveryLoad{RemainingBytes: 5000, PendingResponses: 5, Evidence: DeliveryRecent,
				BytesPerSecond: 10000, Startup: 200 * time.Millisecond}, bytes: block,
			queue: 500 * time.Millisecond, completion: DeliveryEstimate{Evidence: DeliveryRecent, Duration: 600 * time.Millisecond},
		},
		{
			name: "authentication does not occupy the network queue",
			load: DeliveryLoad{PendingResponses: 2, Evidence: DeliveryRecent,
				BytesPerSecond: 10000, Startup: 200 * time.Millisecond}, bytes: block,
			completion: DeliveryEstimate{Evidence: DeliveryRecent, Duration: 300 * time.Millisecond},
		},
		{
			name: "stale evidence remains explicit",
			load: DeliveryLoad{Evidence: DeliveryStale, BytesPerSecond: 1000}, bytes: block,
			completion: DeliveryEstimate{Evidence: DeliveryStale, Duration: time.Second},
		},
		{
			name: "very slow forecasts remain bounded",
			load: DeliveryLoad{RemainingBytes: 1 << 20, PendingResponses: 1,
				Evidence: DeliveryRecent, BytesPerSecond: 1e-6}, bytes: 1,
			queue: MaximumEstimate, completion: DeliveryEstimate{Evidence: DeliveryRecent, Duration: MaximumEstimate},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EstimateQueue(tt.load); got != tt.queue {
				t.Fatalf("network queue: %s, want %s", got, tt.queue)
			}
			if got := EstimateCompletion(tt.load, tt.bytes); got != tt.completion {
				t.Fatalf("completion: %+v, want %+v", got, tt.completion)
			}
		})
	}
}

func TestReadAheadRequiresUsefulMeasuredCapacity(t *testing.T) {
	const block, aggregateRate = 1000, 5000
	tests := []struct {
		name     string
		estimate DeliveryEstimate
		distance uint64
		fits     bool
	}{
		{"frontier has no read-ahead slack", DeliveryEstimate{DeliveryRecent, 100 * time.Millisecond}, 0, false},
		{"independent capacity finishes before consumption", DeliveryEstimate{DeliveryRecent, 500 * time.Millisecond}, 4, true},
		{"slow spare lane would create the next output hole", DeliveryEstimate{DeliveryRecent, 500 * time.Millisecond}, 1, false},
		{"bootstrap is not proven capacity", DeliveryEstimate{DeliveryUnknown, InitialBlockTime}, 4, false},
		{"stale capacity cannot fill read-ahead", DeliveryEstimate{DeliveryStale, 100 * time.Millisecond}, 4, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FitsReadAhead(tt.estimate, block, tt.distance, aggregateRate); got != tt.fits {
				t.Fatalf("read-ahead fit: %v, want %v", got, tt.fits)
			}
		})
	}
}
