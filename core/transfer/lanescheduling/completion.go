package lanescheduling

import "time"

const (
	InitialBlockTime    = 250 * time.Millisecond
	MinimumSampleTime   = time.Millisecond
	MaximumEstimate     = time.Hour
	UserRelayCostFactor = 1.05
	RelayCostFactor     = 1.1
)

// CompletionLoad contains only scheduling evidence. Capacity, route permission,
// and transport ownership are checked by the admission owner.
type CompletionLoad struct {
	PendingBytes   uint64
	PendingBlocks  uint32
	BytesPerSecond float64
}

// EstimateQueue prices existing reservations independently of the next request.
// Without a rate sample, request size cannot establish the speed of old work.
func EstimateQueue(load CompletionLoad) time.Duration {
	seconds := InitialBlockTime.Seconds() * float64(load.PendingBlocks)
	if load.BytesPerSecond > 0 {
		seconds = float64(load.PendingBytes) / load.BytesPerSecond
	}
	return boundedEstimate(seconds, 0)
}

func EstimateCompletion(load CompletionLoad, bytes uint64) time.Duration {
	service := InitialBlockTime.Seconds()
	if load.BytesPerSecond > 0 {
		service = float64(max(bytes, 1)) / load.BytesPerSecond
	}
	return boundedEstimate(EstimateQueue(load).Seconds()+service, MinimumSampleTime)
}

func boundedEstimate(seconds float64, minimum time.Duration) time.Duration {
	return time.Duration(min(max(seconds, minimum.Seconds()), MaximumEstimate.Seconds()) * float64(time.Second))
}

func Cost(estimate time.Duration, factor float64) float64 {
	return float64(estimate) * factor
}
