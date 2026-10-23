package lanescheduling

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/windshare/windshare/core/internal/testvec"
)

type schedulingMetrics struct {
	PendingBytes        uint64  `json:"pendingBytes"`
	PendingBlocks       uint32  `json:"pendingBlocks"`
	QueueMS             float64 `json:"queueMs"`
	CompletionMS        float64 `json:"completionMs"`
	BytesPerSecond      float64 `json:"bytesPerSecond"`
	HasSuccessfulSample bool    `json:"hasSuccessfulSample"`
	BusyMS              float64 `json:"busyMs"`
}

type schedulingEvent struct {
	Event       string            `json:"event"`
	AtMS        int64             `json:"atMs"`
	Lane        string            `json:"lane"`
	Attempt     string            `json:"attempt"`
	Bytes       uint64            `json:"bytes"`
	ExpectedMS  float64           `json:"expectedMs"`
	Outcome     string            `json:"outcome"`
	Alternative string            `json:"alternative"`
	Decision    string            `json:"decision"`
	Expected    schedulingMetrics `json:"expected"`
}

type schedulingAttempt struct {
	lane     *Performance
	started  time.Time
	expected time.Duration
	bytes    uint64
	active   bool
}

type schedulingReplay struct {
	lanes    map[string]*Performance
	attempts map[string]*schedulingAttempt
}

func TestSchedulingBehaviorVectors(t *testing.T) {
	file, err := testvec.Load("../../testvectors/content-scheduling-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if file.Kind != "content-scheduling-v1" || len(file.Cases) == 0 {
		t.Fatal("missing scheduling behavior contract")
	}
	for _, vector := range file.Cases {
		t.Run(vector.Name, func(t *testing.T) {
			var sequence struct {
				Events []schedulingEvent `json:"events"`
			}
			if err := vector.Decode(&sequence); err != nil {
				t.Fatal(err)
			}
			replay := schedulingReplay{lanes: make(map[string]*Performance), attempts: make(map[string]*schedulingAttempt)}
			for index, event := range sequence.Events {
				t.Run(fmt.Sprintf("%02d-%s", index, event.Event), func(t *testing.T) {
					replay.step(t, event)
				})
				if t.Failed() {
					return
				}
			}
		})
	}
}

func (r *schedulingReplay) lane(id string) *Performance {
	if r.lanes[id] == nil {
		r.lanes[id] = &Performance{}
	}
	return r.lanes[id]
}

func (r *schedulingReplay) activeAttempt(t *testing.T, id string) *schedulingAttempt {
	t.Helper()
	attempt := r.attempts[id]
	if attempt == nil || !attempt.active {
		t.Fatalf("attempt %q is not active", id)
	}
	return attempt
}

func (r *schedulingReplay) step(t *testing.T, event schedulingEvent) {
	t.Helper()
	now := time.UnixMilli(event.AtMS)
	switch event.Event {
	case "admit":
		if r.attempts[event.Attempt] != nil {
			t.Fatalf("duplicate attempt %q", event.Attempt)
		}
		lane := r.lane(event.Lane)
		expected := lane.Begin(now, event.Bytes)
		r.attempts[event.Attempt] = &schedulingAttempt{lane: lane, started: now, expected: expected, bytes: event.Bytes, active: true}
		checkSchedulingNumber(t, "admission estimate", float64(expected)/float64(time.Millisecond), event.ExpectedMS)
	case "settle":
		attempt := r.activeAttempt(t, event.Attempt)
		switch event.Outcome {
		case "success", "cancelled", "superseded":
		default:
			t.Fatalf("unknown settlement %q", event.Outcome)
		}
		attempt.lane.Complete(now, attempt.bytes, event.Outcome == "success")
		if event.Outcome == "superseded" {
			attempt.lane.Superseded(attempt.bytes, now.Sub(attempt.started))
		}
		attempt.active = false
	case "inspect":
		checkSchedulingMetrics(t, r.lane(event.Lane), now, event.Bytes, event.Expected)
	case "rescue":
		attempt := r.activeAttempt(t, event.Attempt)
		alternative := r.lane(event.Alternative).EstimateCompletion(attempt.bytes)
		decision := "wait"
		if RescueDue(now.Sub(attempt.started), attempt.expected, alternative) {
			decision = "rescue"
		}
		if decision != event.Decision {
			t.Fatalf("rescue decision = %s, want %s", decision, event.Decision)
		}
	default:
		t.Fatalf("unknown scheduling event %q", event.Event)
	}
}

func checkSchedulingMetrics(t *testing.T, lane *Performance, now time.Time, bytes uint64, expected schedulingMetrics) {
	t.Helper()
	load := lane.Load()
	if load.PendingBytes != expected.PendingBytes || load.PendingBlocks != expected.PendingBlocks ||
		lane.HasSuccessfulSample != expected.HasSuccessfulSample {
		t.Fatalf("evidence = %+v, successful=%v; want %+v", load, lane.HasSuccessfulSample, expected)
	}
	checkSchedulingNumber(t, "queue", float64(lane.EstimateQueue())/float64(time.Millisecond), expected.QueueMS)
	checkSchedulingNumber(t, "completion", float64(lane.EstimateCompletion(bytes))/float64(time.Millisecond), expected.CompletionMS)
	checkSchedulingNumber(t, "throughput", lane.BytesPerSecond, expected.BytesPerSecond)
	checkSchedulingNumber(t, "busy time", float64(lane.BusyTime(now))/float64(time.Millisecond), expected.BusyMS)
}

func checkSchedulingNumber(t *testing.T, name string, actual, expected float64) {
	t.Helper()
	// Go durations resolve to nanoseconds; browser observations use fractional milliseconds.
	const tolerance = 0.000001
	if math.IsNaN(actual) || math.Abs(actual-expected) > tolerance {
		t.Fatalf("%s = %v, want %v", name, actual, expected)
	}
}
