package perfevidence

import (
	"fmt"
	"sort"
)

const (
	defaultBenchTime        = "1s"
	fixedDiskIterations     = "20x"
	fixedTransferIterations = "20x"
	fixedCatalogIterations  = "1x"
	pionFrameBytes          = 64 << 10
	pionLowWaterBytes       = 256 << 10
	pionHighWaterBytes      = 1 << 20
	pionSendAdmissionBytes  = pionHighWaterBytes - 1
	// A send admitted below high-water can add one complete frame before the
	// next bufferedAmount observation, but no additional frame may be admitted.
	pionMaximumAdmittedPeakBytes = pionSendAdmissionBytes + pionFrameBytes
	pionPeakExclusiveLimitBytes  = pionHighWaterBytes + pionFrameBytes
)

var commonMetrics = []string{"ns/op", "B/op", "allocs/op"}

func DefaultWorkloads() []Workload {
	workloads := []Workload{
		readyScalingWorkload(),
		readyDiskWorkload(),
		contentWorkload(),
		multiLaneWorkload(),
		wideCatalogWorkload(),
		relayRegistrationWorkload(),
		pionTransferWorkload(),
	}
	return cloneWorkloads(workloads)
}

func SelectWorkloads(requested []string) ([]Workload, error) {
	available := DefaultWorkloads()
	if len(requested) == 0 {
		return available, nil
	}
	byID := make(map[string]Workload, len(available))
	for _, workload := range available {
		byID[workload.ID] = workload
	}
	selected := make([]Workload, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, id := range requested {
		workload, found := byID[id]
		if !found {
			return nil, fmt.Errorf("unknown performance workload %q", id)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("performance workload %q was selected more than once", id)
		}
		seen[id] = struct{}{}
		selected = append(selected, workload)
	}
	return selected, nil
}

func WorkloadIDs() []string {
	workloads := DefaultWorkloads()
	ids := make([]string, len(workloads))
	for index, workload := range workloads {
		ids[index] = workload.ID
	}
	sort.Strings(ids)
	return ids
}

func readyScalingWorkload() Workload {
	contracts := make([]BenchmarkContract, 0, 5)
	for _, descendants := range []int{0, 1_000, 10_000, 100_000, 1_000_000} {
		contracts = append(contracts, contract(
			fmt.Sprintf("BenchmarkReadyScaling/descendants=%07d", descendants),
			"virtual-descendants", "descendant-fs-ops/op", "registration-material-bytes/op", "descriptor-bytes/op",
		))
	}
	return Workload{
		ID: "ready-scaling", ModuleDir: "core", Package: "./liveshare",
		Benchmark: "BenchmarkReadyScaling", BenchTime: defaultBenchTime, Contracts: contracts,
		HardOracles: []MetricOracle{{
			ID: "ready-does-not-enumerate-descendants", Metric: "descendant-fs-ops/op", Comparison: Equal, Limit: 0,
		}},
	}
}

func readyDiskWorkload() Workload {
	return Workload{
		ID: "ready-real-disk", ModuleDir: "core", Package: "./liveshare",
		Benchmark: "BenchmarkReadyRealDisk", BenchTime: fixedDiskIterations,
		Contracts: []BenchmarkContract{
			contract("BenchmarkReadyRealDisk/path_state=fresh", "registration-material-bytes/op"),
			contract("BenchmarkReadyRealDisk/path_state=reused", "registration-material-bytes/op"),
		},
	}
}

func contentWorkload() Workload {
	contracts := make([]BenchmarkContract, 0, 4)
	for _, blockBytes := range []int{1_024, 64 << 10, 1 << 20, 4 << 20} {
		contracts = append(contracts, contract(
			fmt.Sprintf("BenchmarkFileLocalBlock/block_bytes=%07d", blockBytes),
			"MB/s", "file-local-blocks/op", "sealed-bytes/op", "record-overhead-bytes/op",
		))
	}
	return Workload{
		ID: "content-file-local", ModuleDir: "core", Package: "./content/records",
		Benchmark: "BenchmarkFileLocalBlock", BenchTime: defaultBenchTime, Contracts: contracts,
		HardOracles: []MetricOracle{{
			ID: "one-file-local-block-per-operation", Metric: "file-local-blocks/op", Comparison: Equal, Limit: 1,
		}},
	}
}

func multiLaneWorkload() Workload {
	contracts := make([]BenchmarkContract, 0, 4)
	for _, lanes := range []int{1, 2, 4, 8} {
		contracts = append(contracts, contract(
			fmt.Sprintf("BenchmarkFileLocalMultiLane/lanes=%02d/window=%02d/block_bytes=0065536", lanes, lanes),
			"MB/s", "lane-fetches/op", "duplicate-fetches/op", "window-blocks",
		))
	}
	return Workload{
		ID: "multi-lane", ModuleDir: "core", Package: "./transfer",
		Benchmark: "BenchmarkFileLocalMultiLane", BenchTime: fixedTransferIterations, Contracts: contracts,
		HardOracles: []MetricOracle{{
			ID: "multi-lane-does-not-fetch-duplicates", Metric: "duplicate-fetches/op", Comparison: Equal, Limit: 0,
		}},
	}
}

func wideCatalogWorkload() Workload {
	contracts := make([]BenchmarkContract, 0, 2)
	for _, entries := range []int{10_000, 100_000} {
		contracts = append(contracts, contract(
			fmt.Sprintf("BenchmarkExtremeWidthCatalogSpill/entries=%07d/run_bytes=1048576", entries),
			"entries/op", "pages/op", "sort-spill-written-bytes/op", "sort-object-commits/op",
			"peak-sort-objects", "scan-peak-session-bytes", "retained-catalog-bytes/op",
		))
	}
	return Workload{
		ID: "extreme-width-catalog", ModuleDir: "core", Package: "./catalog",
		Benchmark: "BenchmarkExtremeWidthCatalogSpill", BenchTime: fixedCatalogIterations, Contracts: contracts,
	}
}

func relayRegistrationWorkload() Workload {
	return Workload{
		ID: "relay-registration-wire", ModuleDir: ".", Package: "./transport/relayv2",
		Benchmark: "BenchmarkRelaySenderRegistration", BenchTime: defaultBenchTime,
		Contracts: []BenchmarkContract{contract(
			"BenchmarkRelaySenderRegistration", "registration-wire-sent-B/op", "registration-wire-received-B/op",
			"descriptor-bytes/op", "registration-writes/op", "registration-reads/op",
		)},
		HardOracles: []MetricOracle{
			{ID: "registration-write-count", Metric: "registration-writes/op", Comparison: Equal, Limit: 3},
			{ID: "registration-read-count", Metric: "registration-reads/op", Comparison: Equal, Limit: 2},
		},
	}
}

func pionTransferWorkload() Workload {
	contracts := make([]BenchmarkContract, 0, 4)
	sharedOracles := []MetricOracle{
		{ID: "pion-low-water-matches-production", Metric: "low-water-B", Comparison: Equal, Limit: pionLowWaterBytes},
		{ID: "pion-high-water-matches-production", Metric: "high-water-B", Comparison: Equal, Limit: pionHighWaterBytes},
		{ID: "pion-send-admission-matches-production", Metric: "send-admission-high-water-B", Comparison: Equal, Limit: pionSendAdmissionBytes},
		{ID: "pion-frame-limit-matches-production", Metric: "max-frame-B", Comparison: Equal, Limit: pionFrameBytes},
		{ID: "pion-positive-duration", Metric: "ns/op", Comparison: GreaterThan, Limit: 0},
		{ID: "pion-nonnegative-throughput", Metric: "MB/s", Comparison: GreaterThanOrEqual, Limit: 0},
		{ID: "pion-nonnegative-allocation-bytes", Metric: "B/op", Comparison: GreaterThanOrEqual, Limit: 0},
		{ID: "pion-nonnegative-allocations", Metric: "allocs/op", Comparison: GreaterThanOrEqual, Limit: 0},
		{ID: "pion-nonnegative-buffered-peak", Metric: "peak-buffered-B", Comparison: GreaterThanOrEqual, Limit: 0},
		{ID: "pion-buffer-remains-below-one-frame-high-water", Metric: "peak-buffered-B", Comparison: LessThan, Limit: pionPeakExclusiveLimitBytes},
		{ID: "pion-buffer-respects-admission-plus-one-frame", Metric: "peak-buffered-B", Comparison: LessThanOrEqual, Limit: pionMaximumAdmittedPeakBytes},
	}
	chunkSizes := []int{1 << 10, 64 << 10, 1 << 20, 4 << 20}
	oracles := make([]MetricOracle, 0, len(sharedOracles)+2*len(chunkSizes))
	oracles = append(oracles, sharedOracles...)
	for _, chunkBytes := range chunkSizes {
		kib := chunkBytes >> 10
		name := fmt.Sprintf("BenchmarkPionChunkTransfer/chunk_%dKiB", kib)
		contracts = append(contracts, contract(
			name,
			"MB/s", "frames/chunk", "wire-B/chunk", "peak-buffered-B", "low-water-B",
			"high-water-B", "send-admission-high-water-B", "max-frame-B",
		))
		expectedFrames := (chunkBytes + pionFrameBytes - 1) / pionFrameBytes
		oracles = append(oracles,
			MetricOracle{
				ID: fmt.Sprintf("pion-chunk-%d-wire-geometry", kib), Benchmark: name,
				Metric: "wire-B/chunk", Comparison: Equal, Limit: float64(chunkBytes),
			},
			MetricOracle{
				ID: fmt.Sprintf("pion-chunk-%d-frame-geometry", kib), Benchmark: name,
				Metric: "frames/chunk", Comparison: Equal, Limit: float64(expectedFrames),
			},
		)
	}
	return Workload{
		ID: "pion-chunk-transfer", ModuleDir: ".", Package: "./transport/webrtc",
		Benchmark: "BenchmarkPionChunkTransfer", BenchTime: defaultBenchTime, Contracts: contracts,
		HardOracles: oracles,
	}
}

func contract(name string, metrics ...string) BenchmarkContract {
	required := append(append([]string(nil), commonMetrics...), metrics...)
	return BenchmarkContract{Name: name, RequiredMetrics: required}
}

func cloneWorkloads(source []Workload) []Workload {
	result := make([]Workload, len(source))
	for index, workload := range source {
		result[index] = workload
		result[index].Contracts = append([]BenchmarkContract(nil), workload.Contracts...)
		for contractIndex := range result[index].Contracts {
			result[index].Contracts[contractIndex].RequiredMetrics = append(
				[]string(nil), workload.Contracts[contractIndex].RequiredMetrics...,
			)
		}
		result[index].HardOracles = append([]MetricOracle(nil), workload.HardOracles...)
	}
	return result
}
