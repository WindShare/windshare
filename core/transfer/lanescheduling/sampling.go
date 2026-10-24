package lanescheduling

import "time"

const (
	SampleLifetime             = 12 * time.Second
	SampleByteBudget    uint64 = 8 * 1024 * 1024
	SampleAttemptBudget        = 8
)

type SamplingOutcome string

const (
	SampleCompleted  SamplingOutcome = "completed"
	SampleFailed     SamplingOutcome = "failed"
	SampleSuperseded SamplingOutcome = "superseded"
	SampleCancelled  SamplingOutcome = "cancelled"
)

// SamplingEvidence counts unique assigned work, separately from authenticated
// winners and output. Caller cancellation cannot prove a path lacks capacity.
type SamplingEvidence struct {
	OfferedBytes       uint64
	SettledBytes       uint64
	CompletedBytes     uint64
	CompletedAttempts  uint64
	FailedAttempts     uint64
	SupersededAttempts uint64
	CancelledAttempts  uint64
	BusyMilliseconds   int64
}

type sampleLease struct {
	expires  time.Time
	bytes    uint64
	attempts int
}

type Sampling struct {
	evidence  SamplingEvidence
	pending   uint64
	busySince time.Time
	busy      time.Duration
	lease     *sampleLease
}

func (s *Sampling) Start(now time.Time) {
	s.lease = &sampleLease{expires: now.Add(SampleLifetime), bytes: SampleByteBudget, attempts: SampleAttemptBudget}
}

func (s *Sampling) Stop() { s.lease = nil }

func (s *Sampling) Available(bytes uint64, now time.Time) bool {
	return s.lease != nil && now.Before(s.lease.expires) && s.lease.attempts > 0 && bytes <= s.lease.bytes
}

func (s *Sampling) Offered(bytes uint64, now time.Time) {
	s.evidence.OfferedBytes += bytes
	if s.pending == 0 {
		s.busySince = now
	}
	s.pending++
	// Natural allocation consumes the same allowance as accelerated probes.
	if s.lease != nil {
		s.lease.bytes -= min(s.lease.bytes, bytes)
		s.lease.attempts = max(0, s.lease.attempts-1)
	}
}

func (s *Sampling) Settled(bytes uint64, now time.Time, outcome SamplingOutcome) {
	s.evidence.SettledBytes += bytes
	if outcome == SampleCompleted {
		s.evidence.CompletedBytes += bytes
	}
	switch outcome {
	case SampleCompleted:
		s.evidence.CompletedAttempts++
	case SampleFailed:
		s.evidence.FailedAttempts++
	case SampleSuperseded:
		s.evidence.SupersededAttempts++
	case SampleCancelled:
		s.evidence.CancelledAttempts++
	}
	s.pending--
	if s.pending == 0 {
		s.busy += max(0, now.Sub(s.busySince))
	}
}

func (s *Sampling) Snapshot(now time.Time) SamplingEvidence {
	result := s.evidence
	busy := s.busy
	if s.pending > 0 {
		busy += max(0, now.Sub(s.busySince))
	}
	result.BusyMilliseconds = busy.Milliseconds()
	return result
}
