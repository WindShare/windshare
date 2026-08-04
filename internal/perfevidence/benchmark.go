package perfevidence

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	benchmarkScannerInitialBytes = 64 << 10
	benchmarkScannerMaximumBytes = 4 << 20
)

var benchmarkNamePattern = regexp.MustCompile(`^(Benchmark\S+?)-\d+$`)

func ParseBenchmarkOutput(output []byte) ([]BenchmarkReading, error) {
	var readings []BenchmarkReading
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, benchmarkScannerInitialBytes), benchmarkScannerMaximumBytes)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		nameMatch := benchmarkNamePattern.FindStringSubmatch(fields[0])
		if nameMatch == nil {
			continue
		}
		reading, err := parseBenchmarkFields(nameMatch[1], fields[1:])
		if err != nil {
			return nil, err
		}
		readings = append(readings, reading)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan benchmark output: %w", err)
	}
	if len(readings) == 0 {
		return nil, errors.New("benchmark process produced no parseable benchmark rows")
	}
	return readings, nil
}

func parseBenchmarkFields(name string, fields []string) (BenchmarkReading, error) {
	iterations, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil || iterations == 0 {
		return BenchmarkReading{}, fmt.Errorf("benchmark %s has invalid iteration count %q", name, fields[0])
	}
	if len(fields[1:])%2 != 0 {
		return BenchmarkReading{}, fmt.Errorf("benchmark %s has an incomplete metric pair", name)
	}
	metrics := make(map[string]float64, len(fields[1:])/2)
	for index := 1; index < len(fields); index += 2 {
		value, parseErr := strconv.ParseFloat(fields[index], 64)
		unit := fields[index+1]
		if parseErr != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return BenchmarkReading{}, fmt.Errorf(
				"benchmark %s metric %s has invalid value %q", name, unit, fields[index],
			)
		}
		if _, duplicate := metrics[unit]; duplicate {
			return BenchmarkReading{}, fmt.Errorf("benchmark %s repeats metric %s", name, unit)
		}
		metrics[unit] = value
	}
	return BenchmarkReading{Name: name, Iterations: iterations, Metrics: metrics}, nil
}

func ValidateSample(workload Workload, rows []BenchmarkReading) error {
	if len(rows) != len(workload.Contracts) {
		return fmt.Errorf("workload %s produced %d benchmark rows; want %d", workload.ID, len(rows), len(workload.Contracts))
	}
	byName := make(map[string]BenchmarkReading, len(rows))
	for _, row := range rows {
		if _, duplicate := byName[row.Name]; duplicate {
			return fmt.Errorf("workload %s repeated benchmark %s", workload.ID, row.Name)
		}
		byName[row.Name] = row
	}
	for _, contract := range workload.Contracts {
		row, found := byName[contract.Name]
		if !found {
			return fmt.Errorf("workload %s omitted benchmark %s", workload.ID, contract.Name)
		}
		if len(row.Metrics) != len(contract.RequiredMetrics) {
			return fmt.Errorf(
				"benchmark %s produced %d metrics; contract requires exactly %d",
				contract.Name, len(row.Metrics), len(contract.RequiredMetrics),
			)
		}
		for _, metric := range contract.RequiredMetrics {
			if _, found := row.Metrics[metric]; !found {
				return fmt.Errorf("benchmark %s omitted metric %s", contract.Name, metric)
			}
		}
	}
	return nil
}

func AggregateSamples(samples []BenchmarkSample, expectedSamples int) ([]BenchmarkAggregate, error) {
	if expectedSamples < 1 {
		return nil, errors.New("expected sample count must be positive")
	}
	values := make(map[string]map[string][]float64)
	for _, sample := range samples {
		for _, row := range sample.Rows {
			metrics := values[row.Name]
			if metrics == nil {
				metrics = make(map[string][]float64)
				values[row.Name] = metrics
			}
			for metric, value := range row.Metrics {
				metrics[metric] = append(metrics[metric], value)
			}
		}
	}
	var aggregates []BenchmarkAggregate
	for benchmark, metrics := range values {
		for metric, metricValues := range metrics {
			if len(metricValues) != expectedSamples {
				return nil, fmt.Errorf(
					"benchmark %s metric %s has %d samples; want %d",
					benchmark, metric, len(metricValues), expectedSamples,
				)
			}
			sort.Float64s(metricValues)
			aggregates = append(aggregates, BenchmarkAggregate{
				Benchmark: benchmark,
				Metric:    metric,
				Samples:   len(metricValues),
				Minimum:   metricValues[0],
				P50:       nearestRank(metricValues, 0.50),
				P95:       nearestRank(metricValues, 0.95),
				Maximum:   metricValues[len(metricValues)-1],
			})
		}
	}
	sort.Slice(aggregates, func(left, right int) bool {
		if aggregates[left].Benchmark != aggregates[right].Benchmark {
			return aggregates[left].Benchmark < aggregates[right].Benchmark
		}
		return aggregates[left].Metric < aggregates[right].Metric
	})
	return aggregates, nil
}

func EvaluateOracles(workload Workload, samples []BenchmarkSample) []OracleResult {
	results := make([]OracleResult, 0, len(workload.HardOracles)+1)
	results = append(results, OracleResult{ID: "fresh-process-samples", Passed: freshProcesses(samples)})
	if !results[0].Passed {
		results[0].Error = "samples did not record distinct successful process launches"
	}
	for _, oracle := range workload.HardOracles {
		result := evaluateOracle(oracle, samples)
		results = append(results, result)
	}
	return results
}

func evaluateOracle(oracle MetricOracle, samples []BenchmarkSample) OracleResult {
	limit := oracle.Limit
	result := OracleResult{
		ID: oracle.ID, Benchmark: oracle.Benchmark, Metric: oracle.Metric,
		Comparison: oracle.Comparison, Limit: &limit, Passed: true,
	}
	observations := 0
	for _, sample := range samples {
		for _, row := range sample.Rows {
			if oracle.Benchmark != "" && row.Name != oracle.Benchmark {
				continue
			}
			value, found := row.Metrics[oracle.Metric]
			if !found {
				continue
			}
			observations++
			if !compare(value, oracle.Comparison, oracle.Limit) {
				result.Passed = false
				result.Error = fmt.Sprintf(
					"%s in %s sample %d was %g", oracle.Metric, row.Name, sample.Index, value,
				)
				return result
			}
		}
	}
	if observations == 0 {
		result.Passed = false
		result.Error = "oracle metric was absent from every matching benchmark row"
	}
	return result
}

func OraclesPassed(results []OracleResult) bool {
	for _, result := range results {
		if !result.Passed {
			return false
		}
	}
	return true
}

func nearestRank(sorted []float64, percentile float64) float64 {
	index := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	index = max(index, 0)
	return sorted[index]
}

func freshProcesses(samples []BenchmarkSample) bool {
	if len(samples) == 0 {
		return false
	}
	processes := make(map[int]struct{}, len(samples))
	for _, sample := range samples {
		command := sample.Command
		if sample.Status != OutcomeSucceeded || command.Outcome != OutcomeSucceeded ||
			command.ProcessID <= 0 || !command.FinishedAt.After(command.StartedAt) {
			return false
		}
		if _, duplicate := processes[command.ProcessID]; duplicate {
			return false
		}
		processes[command.ProcessID] = struct{}{}
	}
	return true
}

func compare(value float64, comparison Comparison, limit float64) bool {
	switch comparison {
	case Equal:
		return value == limit
	case LessThan:
		return value < limit
	case LessThanOrEqual:
		return value <= limit
	case GreaterThan:
		return value > limit
	case GreaterThanOrEqual:
		return value >= limit
	default:
		return false
	}
}
