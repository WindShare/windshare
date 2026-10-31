package throughput

import (
	"slices"
	"time"

	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

const (
	SampleInterval             = time.Second
	ObservationWindow          = 3 * time.Second
	FailureBackoff             = 15 * time.Second
	maximumInconclusiveBackoff = time.Minute
	settlementWindow           = lanescheduling.SampleLifetime
	IdleRetention              = 10 * time.Second
	MinimumUsefulGain          = 1.10
	MinimumOutputGain          = 1.05
	MinimumOutputRatio         = 0.65
	minimumSettledAttempts     = 4
	minimumBusyMilliseconds    = 1000
)

type PathIdentity struct {
	PathID    string
	LaneID    uint32
	LaneEpoch uint32
}

type PathObservation struct {
	Identity PathIdentity
	Optional bool
	Work     lanescheduling.SamplingEvidence
}

type Observation struct {
	InFlightBytes                                      uint64
	QueuedBytes, UnfinishedBytes, CapacityWaitingBytes uint64
	NetworkWaitingBytes                                uint64
	UsefulBytes, OutputBytes, RemainingBytes           uint64
	RemainingKnown                                     bool
	Paths                                              []PathObservation
	Pending                                            bool
}

type Action string

const (
	Hold    Action = "hold"
	Explore Action = "explore"
	Sample  Action = "sample"
	Settle  Action = "settle"
	Retire  Action = "retire"
	Keep    Action = "keep"
	Abandon Action = "abandon"
)

type Decision struct {
	Action                                 Action
	Reason                                 string
	Path                                   PathIdentity
	Work                                   lanescheduling.SamplingEvidence
	RetryAfter                             time.Duration
	UsefulRate, OutputRate                 float64
	BaselineUsefulRate, BaselineOutputRate float64
}

type window struct {
	at             time.Time
	useful, output uint64
}
type rates struct{ useful, output float64 }

func windowAt(now time.Time, o Observation) window { return window{now, o.UsefulBytes, o.OutputBytes} }
func ratesAt(now time.Time, o Observation, w window) rates {
	seconds := now.Sub(w.at).Seconds()
	return rates{float64(o.UsefulBytes-w.useful) / seconds, float64(o.OutputBytes-w.output) / seconds}
}
func (r rates) decision(action Action, reason string) Decision {
	return Decision{Action: action, Reason: reason, UsefulRate: r.useful, OutputRate: r.output}
}
func identities(paths []PathObservation) []PathIdentity {
	result := make([]PathIdentity, 0, len(paths))
	for _, path := range paths {
		result = append(result, path.Identity)
	}
	return result
}
func samePaths(a, b []PathIdentity) bool {
	if len(a) != len(b) {
		return false
	}
	for _, path := range a {
		if !slices.Contains(b, path) {
			return false
		}
	}
	return true
}
func newestOptional(paths []PathObservation) *PathObservation {
	for i := len(paths) - 1; i >= 0; i-- {
		if paths[i].Optional {
			return &paths[i]
		}
	}
	return nil
}
