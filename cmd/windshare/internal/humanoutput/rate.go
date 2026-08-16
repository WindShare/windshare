package humanoutput

import (
	"math"
	"math/bits"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
)

const (
	minimumRateWindow   = time.Second
	minimumStableWindow = 2 * time.Second
	maximumRateWindow   = 5 * time.Second
	maximumRateRatio    = uint64(2)
)

type timedCounter struct {
	at    time.Time
	bytes uint64
}

type rateEstimate struct {
	bytesPerSecond uint64
	valid          bool
	stable         bool
}

type rateWindow struct {
	samples []timedCounter
}

func (window *rateWindow) reset() {
	window.samples = nil
}

func (window *rateWindow) observe(now time.Time, snapshot clievent.ProgressSnapshot) rateEstimate {
	current := timedCounter{at: now, bytes: snapshot.NewlyVerifiedBytes()}
	if !snapshot.CountersExact() || snapshot.Discovery() == clievent.DiscoveryFailed {
		window.samples = []timedCounter{current}
		return rateEstimate{}
	}
	if len(window.samples) != 0 {
		last := window.samples[len(window.samples)-1]
		gap := now.Sub(last.at)
		if gap <= 0 || gap > maximumRateWindow || current.bytes < last.bytes {
			window.samples = []timedCounter{current}
			return rateEstimate{}
		}
	}
	window.samples = append(window.samples, current)
	cutoff := now.Add(-maximumRateWindow)
	first := 0
	for first+1 < len(window.samples) && window.samples[first].at.Before(cutoff) {
		first++
	}
	window.samples = window.samples[first:]
	if len(window.samples) < 2 {
		return rateEstimate{}
	}

	oldest := window.samples[0]
	elapsed := now.Sub(oldest.at)
	delta := current.bytes - oldest.bytes
	if elapsed < minimumRateWindow || delta == 0 {
		return rateEstimate{}
	}
	rate := bytesPerSecond(delta, elapsed)
	if rate == 0 {
		return rateEstimate{}
	}
	return rateEstimate{
		bytesPerSecond: rate,
		valid:          true,
		stable:         window.stable(elapsed),
	}
}

func (window *rateWindow) stable(totalElapsed time.Duration) bool {
	if len(window.samples) < 3 || totalElapsed < minimumStableWindow {
		return false
	}
	minimum, maximum := uint64(math.MaxUint64), uint64(0)
	for index := 1; index < len(window.samples); index++ {
		previous, current := window.samples[index-1], window.samples[index]
		delta := current.bytes - previous.bytes
		if delta == 0 {
			return false
		}
		rate := bytesPerSecond(delta, current.at.Sub(previous.at))
		if rate == 0 {
			return false
		}
		minimum = min(minimum, rate)
		maximum = max(maximum, rate)
	}
	// Division avoids overflow when a saturated counter produces a huge rate.
	return maximum/maximumRateRatio <= minimum &&
		(maximum%maximumRateRatio == 0 || maximum/maximumRateRatio < minimum)
}

func bytesPerSecond(delta uint64, elapsed time.Duration) uint64 {
	if delta == 0 || elapsed <= 0 {
		return 0
	}
	high, low := bits.Mul64(delta, uint64(time.Second))
	divisor := uint64(elapsed)
	if high >= divisor {
		return math.MaxUint64
	}
	quotient, _ := bits.Div64(high, low, divisor)
	return quotient
}
