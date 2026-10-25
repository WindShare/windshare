package lanescheduling

import (
	"math"
	"testing"
	"time"

	"github.com/windshare/windshare/core/internal/testvec"
)

type deliveryVectorEstimate struct {
	Evidence     DeliveryEvidence `json:"evidence"`
	Milliseconds float64          `json:"milliseconds"`
}

func (e deliveryVectorEstimate) estimate() DeliveryEstimate {
	return DeliveryEstimate{Evidence: e.Evidence, Duration: time.Duration(e.Milliseconds * float64(time.Millisecond))}
}

type deliveryVector struct {
	Kind          string                 `json:"kind"`
	Estimate      deliveryVectorEstimate `json:"estimate"`
	Distance      uint64                 `json:"distance"`
	AggregateRate float64                `json:"aggregateRate"`
	Fits          bool                   `json:"fits"`
	Load          struct {
		RemainingBytes   uint64 `json:"remainingBytes"`
		PendingResponses uint32 `json:"pendingResponses"`
		Sample           struct {
			Kind                DeliveryEvidence `json:"kind"`
			BytesPerSecond      float64          `json:"bytesPerSecond"`
			StartupMilliseconds float64          `json:"startupMilliseconds"`
		} `json:"sample"`
	} `json:"load"`
	Bytes    uint64                 `json:"bytes"`
	QueueMS  float64                `json:"queueMs"`
	Expected deliveryVectorEstimate `json:"expected"`
	Progress struct {
		RemainingBytes      uint64                 `json:"remainingBytes"`
		ElapsedMilliseconds float64                `json:"elapsedMilliseconds"`
		SilentMilliseconds  float64                `json:"silentMilliseconds"`
		Remaining           deliveryVectorEstimate `json:"remaining"`
	} `json:"progress"`
	Initial     deliveryVectorEstimate `json:"initial"`
	Alternative deliveryVectorEstimate `json:"alternative"`
	Reason      RescueReason           `json:"reason"`
}

func TestSchedulingBehaviorVectors(t *testing.T) {
	file, err := testvec.Load("../../testvectors/content-scheduling-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if file.Kind != "content-scheduling-v1" || len(file.Cases) == 0 {
		t.Fatal("missing scheduling behavior contract")
	}
	for _, entry := range file.Cases {
		t.Run(entry.Name, func(t *testing.T) {
			var vector deliveryVector
			if err := entry.Decode(&vector); err != nil {
				t.Fatal(err)
			}
			switch vector.Kind {
			case "estimate":
				load := DeliveryLoad{RemainingBytes: vector.Load.RemainingBytes, PendingResponses: vector.Load.PendingResponses,
					Evidence: vector.Load.Sample.Kind, BytesPerSecond: vector.Load.Sample.BytesPerSecond,
					Startup: time.Duration(vector.Load.Sample.StartupMilliseconds * float64(time.Millisecond))}
				actual := EstimateCompletion(load, vector.Bytes)
				if actual.Evidence != vector.Expected.Evidence {
					t.Fatalf("evidence: %s", actual.Evidence)
				}
				checkDeliveryDuration(t, actual.Duration, vector.Expected.Milliseconds)
				checkDeliveryDuration(t, EstimateQueue(load), vector.QueueMS)
			case "read-ahead":
				if got := FitsReadAhead(vector.Estimate.estimate(), vector.Bytes, vector.Distance, vector.AggregateRate); got != vector.Fits {
					t.Fatalf("read-ahead fit: %v, want %v", got, vector.Fits)
				}
			case "rescue":
				progress := DeliveryProgress{RemainingBytes: vector.Progress.RemainingBytes,
					Elapsed:   time.Duration(vector.Progress.ElapsedMilliseconds * float64(time.Millisecond)),
					Silent:    time.Duration(vector.Progress.SilentMilliseconds * float64(time.Millisecond)),
					Remaining: vector.Progress.Remaining.estimate()}
				if got := RescueDecision(progress, vector.Initial.estimate(), vector.Alternative.estimate()); got != vector.Reason {
					t.Fatalf("rescue: %q, want %q", got, vector.Reason)
				}
			default:
				t.Fatalf("unknown vector kind %q", vector.Kind)
			}
		})
	}
}
func checkDeliveryDuration(t *testing.T, actual time.Duration, milliseconds float64) {
	t.Helper()
	const tolerance = 0.000001
	if math.Abs(float64(actual)/float64(time.Millisecond)-milliseconds) > tolerance {
		t.Fatalf("duration: %s, want %v ms", actual, milliseconds)
	}
}
