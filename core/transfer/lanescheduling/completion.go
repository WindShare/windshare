package lanescheduling

import "time"

const (
	InitialBlockTime        = 250 * time.Millisecond
	MinimumSampleTime       = time.Millisecond
	EvidenceLifetime        = time.Second
	MaximumEstimate         = time.Hour
	UserRelayCostFactor     = 1.05
	RelayCostFactor         = 1.1
	StaleEvidenceCostFactor = 2
)

type DeliveryEvidence string

const (
	DeliveryUnknown DeliveryEvidence = "unknown"
	DeliveryRecent  DeliveryEvidence = "recent"
	DeliveryStale   DeliveryEvidence = "stale"
)

// DeliveryLoad describes network supply, not authenticated plaintext or retained output.
type DeliveryLoad struct {
	RemainingBytes   uint64
	PendingResponses uint32
	Evidence         DeliveryEvidence
	BytesPerSecond   float64
	Startup          time.Duration
}

type DeliveryEstimate struct {
	Evidence DeliveryEvidence
	Duration time.Duration
}

type DeliveryProgress struct {
	RemainingBytes uint64
	Elapsed        time.Duration
	Silent         time.Duration
	Remaining      DeliveryEstimate
}

// EstimateQueue counts only bytes still expected from the transport.
func EstimateQueue(load DeliveryLoad) time.Duration {
	if load.RemainingBytes == 0 {
		return 0
	}
	if load.BytesPerSecond <= 0 {
		return boundedEstimate(float64(load.PendingResponses)*InitialBlockTime.Seconds(), 0)
	}
	return boundedEstimate(float64(load.RemainingBytes)/load.BytesPerSecond, 0)
}

func EstimateCompletion(load DeliveryLoad, bytes uint64) DeliveryEstimate {
	queue := EstimateQueue(load).Seconds()
	if load.BytesPerSecond <= 0 {
		return DeliveryEstimate{Evidence: DeliveryUnknown, Duration: boundedEstimate(queue+InitialBlockTime.Seconds(), MinimumSampleTime)}
	}
	// New response latency overlaps existing transmission, rather than adding
	// another round trip to every block already in the pipeline.
	seconds := max(queue, load.Startup.Seconds()) + float64(max(bytes, 1))/load.BytesPerSecond
	return DeliveryEstimate{Evidence: load.Evidence, Duration: boundedEstimate(seconds, MinimumSampleTime)}
}

// FitsReadAhead admits spare capacity only when it should finish before consumption.
func FitsReadAhead(estimate DeliveryEstimate, bytes, distance uint64, aggregateRate float64) bool {
	return distance > 0 && estimate.Evidence == DeliveryRecent && aggregateRate > 0 &&
		estimate.Duration.Seconds() <= float64(distance)*float64(bytes)/aggregateRate
}

func CompareDelivery(left, right DeliveryEstimate) int {
	if (left.Evidence == DeliveryUnknown) == (right.Evidence == DeliveryUnknown) {
		return 0
	}
	if left.Evidence != DeliveryUnknown {
		return -1
	}
	return 1
}

func boundedEstimate(seconds float64, minimum time.Duration) time.Duration {
	return time.Duration(min(max(seconds, minimum.Seconds()), MaximumEstimate.Seconds()) * float64(time.Second))
}

// ConservativeDuration permits bounded revalidation without treating old capacity as current.
func ConservativeDuration(estimate DeliveryEstimate) time.Duration {
	if estimate.Evidence == DeliveryStale {
		return min(MaximumEstimate, estimate.Duration*StaleEvidenceCostFactor)
	}
	return estimate.Duration
}

func Cost(estimate time.Duration, factor float64) float64 { return float64(estimate) * factor }
