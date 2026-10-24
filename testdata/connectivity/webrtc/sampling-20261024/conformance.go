package main

import (
	"encoding/json"
	"os"
	"time"

	"github.com/windshare/windshare/connectivity/throughput"
)

type sample struct {
	throughput.Observation
	At int64
}
type scenario struct {
	MaximumPaths int
	Samples      []sample
}

func main() {
	var scenarios []scenario
	if err := json.NewDecoder(os.Stdin).Decode(&scenarios); err != nil {
		panic(err)
	}
	result := make([][]throughput.Decision, 0, len(scenarios))
	for _, input := range scenarios {
		var controller throughput.Controller
		decisions := make([]throughput.Decision, 0, len(input.Samples))
		for _, item := range input.Samples {
			decision := controller.Step(time.Unix(1, 0).Add(time.Duration(item.At)*time.Millisecond), item.Observation, input.MaximumPaths)
			if decision.Action == throughput.Retire || decision.Action == throughput.Sample || decision.Action == throughput.Keep {
				found := false
				for _, path := range item.Paths {
					if path.Identity == decision.Path && path.Optional {
						found = true
					}
				}
				if !found {
					panic("decision targeted an essential or stale generation")
				}
			}
			decisions = append(decisions, decision)
		}
		result = append(result, decisions)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		panic(err)
	}
}
