// Package lanescheduling owns network supply estimates, authenticated delivery evidence, and bounded exploration.
// Route permission and transport liveness remain with their existing owners.
package lanescheduling

import "time"

const (
	MeasurementWindow = time.Second
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
	activeSince         time.Time
	busyTime            time.Duration
}

// Begin accounts for unsettled plaintext, independently of remaining network bytes.
func (p *Performance) Begin(now time.Time, bytes uint64) {
	if p.PendingBytes == 0 {
		p.busySince = now
		p.activeSince = now
		p.completedBytes = 0
	}
	p.PendingBytes += bytes
	p.LastAttempt = now
}

func (p *Performance) Complete(now time.Time, bytes uint64, successful bool) {
	wasBusy := p.PendingBytes != 0
	if successful && bytes != 0 {
		p.recordAuthenticated(now, bytes)
	}
	p.PendingBytes -= min(p.PendingBytes, bytes)
	if p.PendingBytes == 0 && wasBusy {
		p.busyTime += max(now.Sub(p.activeSince), 0)
		p.activeSince = time.Time{}
	}
}

// BusyTime counts overlapping requests once. Paused jobs and output backpressure
// must not look like a slow network simply because wall time kept advancing.
func (p *Performance) BusyTime(now time.Time) time.Duration {
	if p.PendingBytes != 0 {
		return p.busyTime + max(now.Sub(p.activeSince), 0)
	}
	return p.busyTime
}

func (p *Performance) recordAuthenticated(now time.Time, bytes uint64) {
	p.completedBytes += bytes
	elapsed := max(now.Sub(p.busySince), MinimumSampleTime)
	if !p.HasSuccessfulSample || elapsed >= MeasurementWindow || bytes >= p.PendingBytes {
		sample := float64(p.completedBytes) / elapsed.Seconds()
		// Completed-goodput diagnostics reflect sustained drops immediately; growth is
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
