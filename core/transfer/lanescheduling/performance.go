// Package lanescheduling owns content completion estimates and bounded exploration.
// Route permission and transport liveness remain with their existing owners.
package lanescheduling

import "time"

const (
	InitialBlockTime  = 250 * time.Millisecond
	MinimumSampleTime = time.Millisecond
	MeasurementWindow = time.Second
	RelayCostFactor   = 1.1
	sampleWeight      = 0.25
)

// Performance measures authenticated payload over a lane's busy interval, so
// overlapping requests do not each claim the lane's entire throughput.
type Performance struct {
	PendingBytes        uint64
	BytesPerSecond      float64
	HasSuccessfulSample bool
	LastAttempt         time.Time
	busySince           time.Time
	completedBytes      uint64
}

func (p *Performance) Begin(now time.Time, bytes uint64) {
	if p.PendingBytes == 0 {
		p.busySince = now
		p.completedBytes = 0
	}
	p.PendingBytes += bytes
	p.LastAttempt = now
}

func (p *Performance) Complete(now time.Time, bytes uint64, successful bool) {
	if successful && bytes != 0 {
		p.recordAuthenticated(now, bytes)
	}
	p.PendingBytes -= min(p.PendingBytes, bytes)
}

func (p *Performance) recordAuthenticated(now time.Time, bytes uint64) {
	p.completedBytes += bytes
	elapsed := max(now.Sub(p.busySince), MinimumSampleTime)
	if !p.HasSuccessfulSample || elapsed >= MeasurementWindow || bytes >= p.PendingBytes {
		sample := float64(p.completedBytes) / elapsed.Seconds()
		// Congestion should immediately reduce admission. Capacity growth is
		// smoothed so a short burst cannot flood a path with new prefix blocks.
		if p.BytesPerSecond == 0 || sample < p.BytesPerSecond {
			p.BytesPerSecond = sample
		} else {
			p.BytesPerSecond += sampleWeight * (sample - p.BytesPerSecond)
		}
		p.HasSuccessfulSample = true
	}
	if elapsed >= MeasurementWindow {
		p.busySince = now
		p.completedBytes = 0
	}
}

// Superseded is a censored observation, not a path failure. It prevents a lane
// that never finishes before cancellation from retaining an optimistic estimate.
func (p *Performance) Superseded(bytes uint64, elapsed time.Duration) {
	if bytes == 0 || elapsed < MinimumSampleTime {
		return
	}
	upperBound := float64(bytes) / elapsed.Seconds()
	if p.BytesPerSecond == 0 || upperBound < p.BytesPerSecond {
		p.BytesPerSecond = upperBound
	}
}

func (p *Performance) Estimate(bytes uint64) time.Duration {
	bytes = max(bytes, 1)
	seconds := InitialBlockTime.Seconds() * float64(p.PendingBytes+bytes) / float64(bytes)
	if p.BytesPerSecond > 0 {
		seconds = float64(p.PendingBytes+bytes) / p.BytesPerSecond
	}
	// Bound both arithmetic and hedge timers when a path barely makes progress.
	return time.Duration(min(max(seconds, MinimumSampleTime.Seconds()), time.Hour.Seconds()) * float64(time.Second))
}

func Cost(estimate time.Duration, relayed bool) float64 {
	if relayed {
		return float64(estimate) * RelayCostFactor
	}
	return float64(estimate)
}
