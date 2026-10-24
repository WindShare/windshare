package throughput

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestSharedThroughputDecisions(t *testing.T) {
	data, err := os.ReadFile("../../testdata/connectivity/webrtc/throughput-control.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name         string
		MaximumPaths int
		Samples      []struct {
			Observation
			At       int64
			Expected Action
		}
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			var controller Controller
			for _, sample := range test.Samples {
				decision := controller.Step(time.Unix(1, 0).Add(time.Duration(sample.At)*time.Millisecond), sample.Observation, test.MaximumPaths)
				if decision.Action != sample.Expected {
					t.Fatalf("at %d: %+v, want %s", sample.At, decision, sample.Expected)
				}
			}
		})
	}
}
