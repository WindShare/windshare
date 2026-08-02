package perfevidence

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseValidateAndAggregateBenchmarkSamples(t *testing.T) {
	workload := Workload{
		ID: "unit", Contracts: []BenchmarkContract{{
			Name: "BenchmarkUnit/size=1", RequiredMetrics: []string{"ns/op", "B/op", "allocs/op", "objects/op"},
		}},
		HardOracles: []MetricOracle{{ID: "one-object", Metric: "objects/op", Comparison: Equal, Limit: 1}},
	}
	var samples []BenchmarkSample
	for index, duration := range []int{9, 1, 7, 3, 5} {
		output := []byte(fmt.Sprintf(
			"goos: windows\nBenchmarkUnit/size=1-16 100 %d ns/op 8 B/op 2 allocs/op 1 objects/op\nPASS\n",
			duration,
		))
		rows, err := ParseBenchmarkOutput(output)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateSample(workload, rows); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, BenchmarkSample{
			Index: index + 1,
			Command: CommandEvidence{
				ProcessID: index + 10, StartedAt: time.Unix(int64(index), 0), FinishedAt: time.Unix(int64(index), 1),
			},
			Rows: rows,
		})
	}
	aggregates, err := AggregateSamples(samples, 5)
	if err != nil {
		t.Fatal(err)
	}
	var duration BenchmarkAggregate
	for _, aggregate := range aggregates {
		if aggregate.Metric == "ns/op" {
			duration = aggregate
		}
	}
	if duration.Minimum != 1 || duration.P50 != 5 || duration.P95 != 9 || duration.Maximum != 9 {
		t.Fatalf("nearest-rank aggregate = %+v", duration)
	}
	if results := EvaluateOracles(workload, samples); !OraclesPassed(results) {
		t.Fatalf("hard oracles failed: %+v", results)
	}
}

func TestBenchmarkContractRejectsMalformedOrIncompleteEvidence(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{name: "missing rows", output: "PASS\n", want: "no parseable"},
		{name: "invalid iterations", output: "BenchmarkUnit-8 zero 1 ns/op\n", want: "invalid iteration"},
		{name: "unpaired metric", output: "BenchmarkUnit-8 1 1 ns/op 2\n", want: "incomplete metric"},
		{name: "duplicate metric", output: "BenchmarkUnit-8 1 1 ns/op 2 ns/op\n", want: "repeats metric"},
		{name: "invalid number", output: "BenchmarkUnit-8 1 NaN ns/op\n", want: "invalid value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseBenchmarkOutput([]byte(test.output))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	workload := Workload{ID: "unit", Contracts: []BenchmarkContract{{Name: "BenchmarkUnit", RequiredMetrics: []string{"ns/op"}}}}
	if err := ValidateSample(workload, []BenchmarkReading{{Name: "BenchmarkOther", Metrics: map[string]float64{"ns/op": 1}}}); err == nil {
		t.Fatal("missing benchmark contract was accepted")
	}
	if _, err := AggregateSamples(nil, 0); err == nil {
		t.Fatal("zero expected samples were accepted")
	}
}

func TestMetricOracleFailuresAreExplicit(t *testing.T) {
	workload := Workload{
		HardOracles: []MetricOracle{
			{ID: "limit", Metric: "peak", Comparison: LessThan, Limit: 10},
			{ID: "missing", Metric: "absent", Comparison: Equal, Limit: 0},
		},
	}
	samples := []BenchmarkSample{{
		Index:   1,
		Command: CommandEvidence{ProcessID: 1, StartedAt: time.Unix(1, 0), FinishedAt: time.Unix(1, 1)},
		Rows:    []BenchmarkReading{{Name: "BenchmarkUnit", Metrics: map[string]float64{"peak": 10}}},
	}}
	results := EvaluateOracles(workload, samples)
	if OraclesPassed(results) || results[1].Passed || results[2].Passed {
		t.Fatalf("oracle failures = %+v", results)
	}
	if compare(1, Comparison("unsupported"), 1) {
		t.Fatal("unsupported comparison passed")
	}
}

func TestPionOraclesPreserveProductionGeometryAndBufferPolicy(t *testing.T) {
	workload := pionTransferWorkload()
	sample := BenchmarkSample{
		Index: 1,
		Command: CommandEvidence{
			ProcessID: 1, StartedAt: time.Unix(1, 0), FinishedAt: time.Unix(1, 1),
		},
	}
	for _, chunkBytes := range []int{1 << 10, 64 << 10, 1 << 20, 4 << 20} {
		name := fmt.Sprintf("BenchmarkPionChunkTransfer/chunk_%dKiB", chunkBytes>>10)
		sample.Rows = append(sample.Rows, BenchmarkReading{
			Name: name,
			Metrics: map[string]float64{
				"ns/op": 1, "MB/s": 1, "B/op": 0, "allocs/op": 0,
				"frames/chunk": float64((chunkBytes + pionFrameBytes - 1) / pionFrameBytes),
				"wire-B/chunk": float64(chunkBytes), "peak-buffered-B": pionMaximumAdmittedPeakBytes,
				"low-water-B": pionLowWaterBytes, "high-water-B": pionHighWaterBytes,
				"send-admission-high-water-B": pionSendAdmissionBytes, "max-frame-B": pionFrameBytes,
			},
		})
	}
	if results := EvaluateOracles(workload, []BenchmarkSample{sample}); !OraclesPassed(results) {
		t.Fatalf("valid Pion evidence failed: %+v", results)
	}

	tests := []struct {
		name   string
		metric string
		value  float64
	}{
		{name: "chunk geometry", metric: "frames/chunk", value: 2},
		{name: "production constant", metric: "high-water-B", value: pionHighWaterBytes + 1},
		{name: "admission bound", metric: "peak-buffered-B", value: pionMaximumAdmittedPeakBytes + 1},
		{name: "required metric domain", metric: "ns/op", value: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := sample.Rows[0].Metrics[test.metric]
			sample.Rows[0].Metrics[test.metric] = test.value
			defer func() { sample.Rows[0].Metrics[test.metric] = original }()
			if results := EvaluateOracles(workload, []BenchmarkSample{sample}); OraclesPassed(results) {
				t.Fatalf("%s=%g was accepted", test.metric, test.value)
			}
		})
	}
}

func TestWorkloadSelectionIsValidatedAndCloned(t *testing.T) {
	selected, err := SelectWorkloads([]string{"ready-scaling", "pion-chunk-transfer"})
	if err != nil || len(selected) != 2 {
		t.Fatalf("selected = %+v, err = %v", selected, err)
	}
	selected[0].Contracts[0].RequiredMetrics[0] = "mutated"
	fresh, err := SelectWorkloads([]string{"ready-scaling"})
	if err != nil || fresh[0].Contracts[0].RequiredMetrics[0] != "ns/op" {
		t.Fatalf("workload registry leaked mutation: %+v, err = %v", fresh, err)
	}
	if _, err := SelectWorkloads([]string{"missing"}); err == nil {
		t.Fatal("unknown workload was accepted")
	}
	if _, err := SelectWorkloads([]string{"ready-scaling", "ready-scaling"}); err == nil {
		t.Fatal("duplicate workload was accepted")
	}
	if len(WorkloadIDs()) != len(DefaultWorkloads()) {
		t.Fatal("workload ID inventory is incomplete")
	}
}
