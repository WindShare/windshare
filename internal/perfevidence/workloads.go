package perfevidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
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
		BenchmarkHarnessPackages: []string{"github.com/windshare/windshare/core/liveshare"},
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
	oracles := make([]MetricOracle, 0, 19)
	oracles = append(oracles, []MetricOracle{
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
	}...)
	for _, chunkBytes := range []int{1 << 10, 64 << 10, 1 << 20, 4 << 20} {
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
		result[index].BenchmarkHarnessPackages = append(
			[]string(nil), workload.BenchmarkHarnessPackages...,
		)
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

func discoverAndPrefetchWorkloads(
	ctx context.Context,
	runner CommandRunner,
	environment controlledGoEnvironment,
	repositoryRoot string,
	runtimeRoot string,
	workloads []Workload,
) (map[string]workloadOverlay, error) {
	overlays := make(map[string]workloadOverlay, len(workloads))
	for _, workload := range workloads {
		overlay, err := discoverWorkloadOverlay(
			ctx, runner, environment, repositoryRoot, runtimeRoot, workload,
		)
		if err != nil {
			return nil, err
		}
		overlays[workload.ID] = overlay
		// Network access is confined to dependency acquisition; every identity
		// traversal below is offline and starts only after module verification.
		if _, err := listWorkloadGraph(
			ctx, runner, environment, repositoryRoot, workload, overlay, true,
		); err != nil {
			return nil, fmt.Errorf("prefetch workload %s dependencies: %w", workload.ID, err)
		}
	}
	if err := verifyDownloadedModules(ctx, runner, environment, repositoryRoot, workloads); err != nil {
		return nil, err
	}
	return overlays, nil
}

func inventoryLiveWorkloads(
	ctx context.Context,
	runner CommandRunner,
	environment controlledGoEnvironment,
	repositoryRoot string,
	workloads []Workload,
	overlays map[string]workloadOverlay,
) (map[string]workloadInventory, error) {
	inventories := make(map[string]workloadInventory, len(workloads))
	for _, workload := range workloads {
		inventory, err := listWorkloadGraph(
			ctx, runner, environment, repositoryRoot,
			workload, overlays[workload.ID], false,
		)
		if err != nil {
			return nil, fmt.Errorf("inventory workload %s: %w", workload.ID, err)
		}
		inventories[workload.ID] = inventory
	}
	return inventories, nil
}

func materializeSnapshotWorkloads(
	repositoryRoot string,
	artifactRoot string,
	liveInventories map[string]workloadInventory,
	workloads []Workload,
	overlays map[string]workloadOverlay,
) (
	string,
	string,
	[]inventoryFile,
	map[string]PreparedWorkload,
	map[string]workloadOverlay,
	error,
) {
	snapshotRoot := filepath.Join(artifactRoot, snapshotDirectoryName)
	workspaceRoot := filepath.Join(snapshotRoot, "workspace")
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		return "", "", nil, nil, nil, fmt.Errorf("create source snapshot: %w", err)
	}
	union, err := materializeWorkspace(workspaceRoot, liveInventories)
	if err != nil {
		return "", "", nil, nil, nil, err
	}
	if err := materializeWorkspaceManifests(repositoryRoot, workspaceRoot, &union); err != nil {
		return "", "", nil, nil, nil, err
	}
	prepared := make(map[string]PreparedWorkload, len(workloads))
	finalOverlays := make(map[string]workloadOverlay, len(workloads))
	for _, workload := range workloads {
		finalOverlay, err := materializeOverlay(
			repositoryRoot, workspaceRoot, snapshotRoot,
			filepath.Join(workspaceRoot, filepath.FromSlash(workload.ModuleDir)), overlays[workload.ID],
		)
		if err != nil {
			return "", "", nil, nil, nil, err
		}
		finalOverlays[workload.ID] = finalOverlay
		prepared[workload.ID] = PreparedWorkload{
			ModuleRoot:  filepath.Join(workspaceRoot, filepath.FromSlash(workload.ModuleDir)),
			Package:     workload.Package,
			OverlayPath: finalOverlay.OverlayPath,
		}
	}
	return snapshotRoot, workspaceRoot, union, prepared, finalOverlays, nil
}

func verifyLiveClosuresUnchanged(
	ctx context.Context,
	runner CommandRunner,
	environment controlledGoEnvironment,
	repositoryRoot string,
	workloads []Workload,
	overlays map[string]workloadOverlay,
	before map[string]workloadInventory,
) error {
	for _, workload := range workloads {
		after, err := listWorkloadGraph(
			ctx, runner, environment, repositoryRoot,
			workload, overlays[workload.ID], false,
		)
		if err != nil {
			return fmt.Errorf("repeat workload %s inventory: %w", workload.ID, err)
		}
		if after.Closure != before[workload.ID].Closure {
			return fmt.Errorf("workload %s source closure changed while snapshotting", workload.ID)
		}
	}
	return nil
}

func inventorySealedWorkloads(
	ctx context.Context,
	runner CommandRunner,
	environment controlledGoEnvironment,
	processEnvironment []string,
	artifactRoot string,
	workspaceRoot string,
	workloads []Workload,
	overlays map[string]workloadOverlay,
	liveInventories map[string]workloadInventory,
	prepared map[string]PreparedWorkload,
) (map[string]PreparedWorkload, []inventoryFile, []ModuleIdentity, []BuildGraphIdentity, error) {
	graphs := make([]BuildGraphIdentity, 0, len(workloads))
	var files []inventoryFile
	moduleByKey := make(map[string]ModuleIdentity)
	for _, workload := range workloads {
		inventory, err := listWorkloadGraphWithEnvironment(
			ctx, runner, environment, processEnvironment, workspaceRoot,
			workload, overlays[workload.ID],
		)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("verify sealed workload %s graph: %w", workload.ID, err)
		}
		if inventory.Closure != liveInventories[workload.ID].Closure {
			return nil, nil, nil, nil, fmt.Errorf(
				"workload %s snapshot closure %s differs from live closure %s",
				workload.ID, inventory.Closure, liveInventories[workload.ID].Closure,
			)
		}
		files = append(files, inventory.Files...)
		for _, module := range inventory.Modules {
			moduleByKey[moduleIdentityKey(module)] = module
		}
		graph, err := buildGraphIdentity(artifactRoot, overlays[workload.ID], inventory)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		graphs = append(graphs, graph)
		plan := prepared[workload.ID]
		plan.Graph = graph
		prepared[workload.ID] = plan
	}
	modules := make([]ModuleIdentity, 0, len(moduleByKey))
	for _, module := range moduleByKey {
		modules = append(modules, module)
	}
	sort.Slice(modules, func(left, right int) bool {
		return moduleIdentityKey(modules[left]) < moduleIdentityKey(modules[right])
	})
	sort.Slice(graphs, func(left, right int) bool { return graphs[left].WorkloadID < graphs[right].WorkloadID })
	return prepared, files, modules, graphs, nil
}

func listWorkloadGraph(
	ctx context.Context,
	runner CommandRunner,
	environment controlledGoEnvironment,
	workspaceRoot string,
	workload Workload,
	overlay workloadOverlay,
	prefetch bool,
) (workloadInventory, error) {
	return listWorkloadGraphWithEnvironment(
		ctx, runner, environment,
		environment.withWorkspace(filepath.Join(workspaceRoot, "go.work"), prefetch),
		workspaceRoot, workload, overlay,
	)
}

func listWorkloadGraphWithEnvironment(
	ctx context.Context,
	runner CommandRunner,
	environment controlledGoEnvironment,
	processEnvironment []string,
	workspaceRoot string,
	workload Workload,
	overlay workloadOverlay,
) (workloadInventory, error) {
	moduleRoot := filepath.Join(workspaceRoot, filepath.FromSlash(workload.ModuleDir))
	result, err := runControlled(ctx, runner, Command{
		Executable: environment.GoExecutable,
		Arguments: []string{
			"list", "-mod=readonly", "-deps", "-test", "-json", "-overlay", overlay.OverlayPath, workload.Package,
		},
		Directory: moduleRoot, Environment: processEnvironment, ReplaceEnvironment: true,
	})
	if err != nil {
		return workloadInventory{}, err
	}
	packages, err := decodeGoListPackages(commandStdout(result))
	if err != nil {
		return workloadInventory{}, err
	}
	context := inventoryContext{
		RepositoryRoot: workspaceRoot, WorkspaceRoot: workspaceRoot,
		GoRoot: environment.ToolchainLocations.GoRoot, GoModCache: environment.GoModCache,
		GoCache: environment.GoCache, Temporary: environment.Temporary, Overlay: overlay,
	}
	inventory, err := inventoryPackages(packages, context)
	if err != nil {
		return workloadInventory{}, err
	}
	return includeWorkspaceManifests(inventory, workspaceRoot, overlay)
}

func includeWorkspaceManifests(
	inventory workloadInventory,
	workspaceRoot string,
	overlay workloadOverlay,
) (workloadInventory, error) {
	byLogical := make(map[string]inventoryFile, len(inventory.Files)+6)
	for _, file := range inventory.Files {
		byLogical[file.Logical] = file
	}
	for _, relative := range []string{"go.work", "go.work.sum", "go.mod", "go.sum", "core/go.mod", "core/go.sum"} {
		path := filepath.Join(workspaceRoot, filepath.FromSlash(relative))
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return workloadInventory{}, fmt.Errorf("inspect workspace manifest %s: %w", relative, err)
		}
		sha, err := hashFileExact(path, info.Size())
		if err != nil {
			return workloadInventory{}, err
		}
		logical := "workspace/" + filepath.ToSlash(relative)
		byLogical[logical] = inventoryFile{
			Logical: logical, Physical: path, Origin: "workspace",
			WorkspaceRelative: filepath.ToSlash(relative), Mode: info.Mode(), Bytes: info.Size(), SHA256: sha,
		}
	}
	inventory.Files = inventory.Files[:0]
	for _, file := range byLogical {
		inventory.Files = append(inventory.Files, file)
	}
	sort.Slice(inventory.Files, func(left, right int) bool { return inventory.Files[left].Logical < inventory.Files[right].Logical })
	closure, err := closureSHA(inventory.Files, inventory.Modules, inventory.Packages, overlay)
	if err != nil {
		return workloadInventory{}, err
	}
	inventory.Closure = closure
	return inventory, nil
}

func decodeGoListPackages(encoded []byte) ([]goListPackage, error) {
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	var packages []goListPackage
	for {
		var pkg goListPackage
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list package: %w", err)
		}
		packages = append(packages, pkg)
	}
	if len(packages) == 0 {
		return nil, errors.New("go list returned no packages")
	}
	return packages, nil
}

func requiredIdentityErrors(host HostMetadata, source SnapshotIdentity) []string {
	errorsByID := make(map[string]struct{}, len(host.RequiredErrors)+8)
	for _, issue := range host.RequiredErrors {
		errorsByID[issue] = struct{}{}
	}
	if source.SHA256 == "" {
		errorsByID["source-snapshot-identity-missing"] = struct{}{}
	}
	toolchain := source.Toolchain
	toolchainLocations := source.Diagnostics.Toolchain
	if len(toolchain.ExecutableSHA256) != 64 || toolchain.Version == "" || toolchain.GoVersion == "" ||
		len(toolchain.Tools) == 0 || len(toolchain.BuildInputs) == 0 || toolchainLocations.ExecutablePath == "" ||
		toolchainLocations.GoRoot == "" || toolchainLocations.GoToolDir == "" {
		errorsByID["go-toolchain-identity-incomplete"] = struct{}{}
	}
	requiredEnvironment := map[string]string{
		"CGO_ENABLED":  "0",
		"GOENV":        "off",
		"GOEXPERIMENT": "",
		"GOFLAGS":      "",
		"GOOS":         runtime.GOOS,
		"GOARCH":       runtime.GOARCH,
		"GOPROXY":      "off",
		"GOSUMDB":      "off",
		"GOTOOLCHAIN":  "local",
	}
	observed := make(map[string]string, len(source.Diagnostics.ProcessEnvironment))
	for _, variable := range source.Diagnostics.ProcessEnvironment {
		observed[variable.Name] = variable.Value
	}
	for name, expected := range requiredEnvironment {
		if value, found := observed[name]; !found || value != expected {
			errorsByID["controlled-go-environment-incomplete"] = struct{}{}
		}
	}
	for _, required := range []string{"GOCACHE", "GOMODCACHE", "GOWORK", "TEMP"} {
		if observed[required] == "" {
			errorsByID["controlled-go-environment-incomplete"] = struct{}{}
		}
	}
	if len(source.Diagnostics.EffectiveGoEnv) == 0 {
		errorsByID["effective-go-environment-missing"] = struct{}{}
	}
	result := make([]string, 0, len(errorsByID))
	for issue := range errorsByID {
		result = append(result, issue)
	}
	sort.Strings(result)
	return result
}
