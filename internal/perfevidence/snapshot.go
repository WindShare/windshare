package perfevidence

import (
	"bufio"
	"context"
	"crypto/sha1" // Git SHA-1 repositories require the native object algorithm, not a security primitive.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const snapshotDirectoryName = "source-snapshot"

type PreparedWorkload struct {
	ModuleRoot  string
	Package     string
	OverlayPath string
	Graph       BuildGraphIdentity
}

type PreparedSnapshot struct {
	Root        string
	Environment controlledGoEnvironment
	Identity    SnapshotIdentity
	Workloads   map[string]PreparedWorkload
	revalidator snapshotRevalidator
	authority   byteConsumptionAuthority
	domain      MutationDomain
}

type inventoryFile struct {
	Logical           string
	Physical          string
	Origin            string
	WorkspaceRelative string
	Mode              os.FileMode
	Bytes             int64
	SHA256            string
}

type workloadInventory struct {
	Files    []inventoryFile
	Modules  []ModuleIdentity
	Packages []string
	Closure  string
}

type inventoryContext struct {
	RepositoryRoot string
	WorkspaceRoot  string
	GoRoot         string
	GoModCache     string
	GoCache        string
	Temporary      string
	Overlay        workloadOverlay
}

func prepareSnapshot(
	ctx context.Context,
	runner CommandRunner,
	repositoryRoot string,
	artifactRoot string,
	runtimeRoot string,
	workloads []Workload,
	mutationDomains MutationDomainFactory,
) (result PreparedSnapshot, resultErr error) {
	var authority byteConsumptionAuthority
	var domain MutationDomain
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, closeConsumptionAuthority(authority))
			if domain != nil {
				resultErr = errors.Join(resultErr, domain.Close())
			}
		}
	}()
	environment, err := prepareControlledGoEnvironment(ctx, runner, repositoryRoot, runtimeRoot)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	authority = environment.Authority
	gitIdentity, err := CaptureSource(ctx, runner, repositoryRoot)
	if err != nil {
		return PreparedSnapshot{}, fmt.Errorf("capture Git identity: %w", err)
	}

	overlays, err := discoverAndPrefetchWorkloads(
		ctx, runner, environment, repositoryRoot, runtimeRoot, workloads,
	)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	environment, err = isolateControlledGoEnvironment(environment, runtimeRoot)
	if err != nil {
		return PreparedSnapshot{}, err
	}

	liveBefore, err := inventoryLiveWorkloads(
		ctx, runner, environment, repositoryRoot, workloads, overlays,
	)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	if err := validateWorkloadInventoryBudget(liveBefore); err != nil {
		return PreparedSnapshot{}, err
	}

	snapshotRoot, workspaceRoot, union, prepared, finalOverlays, err := materializeSnapshotWorkloads(
		repositoryRoot, artifactRoot, liveBefore, workloads, overlays,
	)
	if err != nil {
		return PreparedSnapshot{}, err
	}

	// A second live traversal closes the materialization window. A mutation is
	// never tolerated merely because it was later reverted in the worktree.
	if err := verifyLiveClosuresUnchanged(
		ctx, runner, environment, repositoryRoot, workloads, overlays, liveBefore,
	); err != nil {
		return PreparedSnapshot{}, err
	}
	if err := requireStableSourceObservation(
		ctx, runner, repositoryRoot, gitIdentity, "snapshot materialization",
	); err != nil {
		return PreparedSnapshot{}, err
	}

	buildCache := filepath.Join(runtimeRoot, "sealed-build-cache")
	if err := os.Mkdir(buildCache, 0o700); err != nil {
		return PreparedSnapshot{}, fmt.Errorf("create sealed build cache: %w", err)
	}
	environment.GoCache = buildCache
	environment.Offline = replaceEnvironmentValue(environment.Offline, "GOCACHE", buildCache)
	environment.Evidence = replaceEvidenceEnvironment(environment.Evidence, "GOCACHE", buildCache)
	snapshotWork := filepath.Join(workspaceRoot, "go.work")
	snapshotEnvironment := environment.withWorkspace(snapshotWork, false)
	if err := sealSnapshotTree(snapshotRoot); err != nil {
		return PreparedSnapshot{}, err
	}
	if err := refreshInventoryModes(union); err != nil {
		return PreparedSnapshot{}, err
	}
	consumptionFiles, err := inventoryConsumptionUniverse(map[string]string{
		"snapshot":   snapshotRoot,
		"goroot":     environment.ToolchainLocations.GoRoot,
		"gomodcache": environment.GoModCache,
	})
	if err != nil {
		return PreparedSnapshot{}, err
	}
	consumptionInputs, err := canonicalSourceFiles(consumptionFiles)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	authorityRoots := append([]string{snapshotRoot}, environment.AuthorityRoots...)
	finalAuthority, err := acquireConsumptionAuthority(inventoryValidationTargets(consumptionFiles), authorityRoots)
	if err != nil {
		return PreparedSnapshot{}, fmt.Errorf("acquire immutable consumption authority: %w", err)
	}
	authority = combineConsumptionAuthorities(authority, finalAuthority)
	environment.Authority = authority
	authoritativeRunner := runner
	if mutationDomains != nil {
		domain, err = mutationDomains.Open(ctx, MutationDomainSpec{
			RuntimeRoot: runtimeRoot,
			Roots: []MutationRoot{
				{Name: "snapshot", HostPath: snapshotRoot},
				{Name: "goroot", HostPath: environment.ToolchainLocations.GoRoot},
				{Name: "gomodcache", HostPath: environment.GoModCache},
			},
		})
		if err != nil {
			return PreparedSnapshot{}, fmt.Errorf("open authoritative snapshot mutation domain: %w", err)
		}
		authoritativeRunner, err = runnerWithMutationDomain(runner, domain)
		if err != nil {
			return PreparedSnapshot{}, err
		}
		if err := verifyDownloadedModulesUnderAuthority(
			ctx, authoritativeRunner, environment, workspaceRoot, workloads, authority,
		); err != nil {
			return PreparedSnapshot{}, fmt.Errorf("verify modules in authoritative mutation domain: %w", err)
		}
	}
	prepared, finalFiles, modules, graphs, err := inventorySealedWorkloads(
		ctx, authoritativeRunner, environment, snapshotEnvironment, artifactRoot, workspaceRoot,
		workloads, finalOverlays, liveBefore, prepared,
	)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	finalFiles = append(finalFiles, union...)
	files, err := canonicalSourceFiles(finalFiles)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	committed, uncommitted, err := classifyCommittedInputs(
		ctx, runner, repositoryRoot, gitIdentity.Commit, files, workspaceRoot,
	)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	effective, err := inspectEffectiveGoEnvironment(
		ctx, authoritativeRunner, environment.GoExecutable, workspaceRoot, snapshotEnvironment,
	)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	controlledEnvironment := replaceEvidenceEnvironment(environment.Evidence, "GOWORK", snapshotWork)
	controlledEnvironment = replaceEvidenceEnvironment(controlledEnvironment, "GOCACHE", buildCache)
	buildEnvironment, err := comparableBuildEnvironment(controlledEnvironment, map[string]semanticPath{
		"GOCACHE":    {Physical: buildCache, Logical: "$RUNTIME/sealed-build-cache"},
		"GOMODCACHE": {Physical: environment.GoModCache, Logical: "$RUNTIME/gomodcache"},
		"GOWORK":     {Physical: snapshotWork, Logical: "$SNAPSHOT/workspace/go.work"},
		"TEMP":       {Physical: environment.Temporary, Logical: "$RUNTIME/tmp"},
		"TMP":        {Physical: environment.Temporary, Logical: "$RUNTIME/tmp"},
		"TMPDIR":     {Physical: environment.Temporary, Logical: "$RUNTIME/tmp"},
	})
	if err != nil {
		return PreparedSnapshot{}, err
	}
	processEnvironment, err := processEnvironmentEvidence(snapshotEnvironment)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	overlayFiles, err := overlayFileDiagnostics(artifactRoot, finalOverlays)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	identity := SnapshotIdentity{
		Git: gitIdentity, Files: committed, ConsumptionInputs: consumptionInputs,
		CompiledInputsMatchCommit: len(uncommitted) == 0,
		UncommittedInputs:         uncommitted,
		BuildEnvironment:          buildEnvironment,
		Toolchain:                 environment.Toolchain,
		Modules:                   modules,
		BuildGraphs:               graphs,
		Diagnostics: SnapshotDiagnostics{
			ProcessEnvironment: processEnvironment,
			EffectiveGoEnv:     effective,
			Toolchain:          environment.ToolchainLocations,
			OverlayFiles:       overlayFiles,
		},
	}
	identity.SHA256, err = snapshotIdentitySHA(identity)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	validationInputs := append(append([]inventoryFile(nil), finalFiles...), consumptionFiles...)
	revalidator, err := newSnapshotValidationPlan(artifactRoot, validationInputs, identity, environment)
	if err != nil {
		return PreparedSnapshot{}, fmt.Errorf("prepare final-byte validation: %w", err)
	}
	if err := requireStableSourceObservation(
		ctx, runner, repositoryRoot, gitIdentity, "snapshot identity finalization",
	); err != nil {
		return PreparedSnapshot{}, err
	}
	environment.Offline = snapshotEnvironment
	environment.Effective = effective
	result = PreparedSnapshot{
		Root: snapshotRoot, Environment: environment, Identity: identity, Workloads: prepared,
		revalidator: revalidator, authority: authority, domain: domain,
	}
	return result, nil
}

func inventoryValidationTargets(files []inventoryFile) []snapshotValidationTarget {
	targets := make([]snapshotValidationTarget, 0, len(files))
	for _, file := range files {
		targets = append(targets, snapshotValidationTarget{
			LogicalPath: file.Logical, PhysicalPath: file.Physical, Bytes: file.Bytes, SHA256: file.SHA256,
		})
	}
	return targets
}

func validateWorkloadInventoryBudget(inventories map[string]workloadInventory) error {
	meter, err := newSnapshotInputMeter(defaultSnapshotInputBudget())
	if err != nil {
		return err
	}
	seen := make(map[string]struct{})
	workloadIDs := make([]string, 0, len(inventories))
	for workloadID := range inventories {
		workloadIDs = append(workloadIDs, workloadID)
	}
	sort.Strings(workloadIDs)
	for _, workloadID := range workloadIDs {
		for _, file := range inventories[workloadID].Files {
			key := canonicalPath(file.Physical)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			if err := meter.observe(file.Logical, true, file.Bytes); err != nil {
				return fmt.Errorf("workload %s input budget: %w", workloadID, err)
			}
		}
	}
	return nil
}

func refreshInventoryModes(files []inventoryFile) error {
	for index := range files {
		information, err := os.Lstat(files[index].Physical)
		if err != nil || !information.Mode().IsRegular() || isReparsePointInfo(information) {
			return errors.Join(fmt.Errorf("refresh sealed input mode for %s", files[index].Logical), err)
		}
		files[index].Mode = information.Mode()
	}
	return nil
}

func runnerWithMutationDomain(runner CommandRunner, domain MutationDomain) (CommandRunner, error) {
	switch concrete := runner.(type) {
	case ProcessRunner:
		concrete.MutationDomain = domain
		return concrete, nil
	case *ProcessRunner:
		copy := *concrete
		copy.MutationDomain = domain
		return copy, nil
	case interface {
		withMutationDomain(MutationDomain) CommandRunner
	}:
		return concrete.withMutationDomain(domain), nil
	default:
		return nil, errors.New("authoritative snapshot mutation domain requires the production process runner")
	}
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

func inventoryPackages(packages []goListPackage, context inventoryContext) (workloadInventory, error) {
	if err := validatePerformanceDependencyDelta(packages, context); err != nil {
		return workloadInventory{}, err
	}
	replacements := make(map[string]overlayReplacement, len(context.Overlay.Replacements))
	for _, replacement := range context.Overlay.Replacements {
		replacements[canonicalPath(replacement.SourcePath)] = replacement
	}
	filesByLogical := make(map[string]inventoryFile)
	modules := make(map[string]ModuleIdentity)
	packageNames := make(map[string]struct{})
	for _, pkg := range packages {
		if pkg.ImportPath != "" {
			packageNames[pkg.ImportPath] = struct{}{}
		}
		if err := recordPackageModule(pkg, context, modules); err != nil {
			return workloadInventory{}, err
		}
		if err := inventoryPackageFiles(pkg, context, replacements, filesByLogical); err != nil {
			return workloadInventory{}, err
		}
	}
	files := make([]inventoryFile, 0, len(filesByLogical))
	for _, file := range filesByLogical {
		files = append(files, file)
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Logical < files[right].Logical })
	moduleList := make([]ModuleIdentity, 0, len(modules))
	for _, module := range modules {
		moduleList = append(moduleList, module)
	}
	sort.Slice(moduleList, func(left, right int) bool {
		return moduleIdentityKey(moduleList[left]) < moduleIdentityKey(moduleList[right])
	})
	packageList := make([]string, 0, len(packageNames))
	for name := range packageNames {
		packageList = append(packageList, name)
	}
	sort.Strings(packageList)
	closure, err := closureSHA(files, moduleList, packageList, context.Overlay)
	if err != nil {
		return workloadInventory{}, err
	}
	return workloadInventory{Files: files, Modules: moduleList, Packages: packageList, Closure: closure}, nil
}

func validatePerformanceDependencyDelta(packages []goListPackage, context inventoryContext) error {
	targetImport := context.Overlay.PackageImport
	normalPackages := make(map[string]goListPackage, len(packages))
	for _, pkg := range packages {
		if pkg.ForTest == "" && pkg.ImportPath != "" {
			normalPackages[pkg.ImportPath] = pkg
		}
	}
	target, found := normalPackages[targetImport]
	if !found {
		return fmt.Errorf("performance graph omitted production package %s", targetImport)
	}
	productionClosure := make(map[string]struct{}, len(target.Deps)+1)
	productionClosure[targetImport] = struct{}{}
	for _, dependency := range target.Deps {
		productionClosure[dependency] = struct{}{}
	}
	allowed := make(map[string]struct{}, len(productionClosure))
	for importPath := range productionClosure {
		allowed[importPath] = struct{}{}
	}
	for _, declared := range context.Overlay.BenchmarkHarnessPackages {
		harness, present := normalPackages[declared]
		if !present {
			return fmt.Errorf("declared benchmark harness package %s is absent from the performance graph", declared)
		}
		if harness.Standard || !repositoryLocalGoPackage(harness, context.WorkspaceRoot) {
			return fmt.Errorf("declared benchmark harness package %s is not repository-local", declared)
		}
		if _, redundant := productionClosure[declared]; redundant {
			return fmt.Errorf("declared benchmark harness package %s is already in the production closure", declared)
		}
		allowed[declared] = struct{}{}
		for _, dependency := range harness.Deps {
			allowed[dependency] = struct{}{}
		}
	}
	violations := make(map[string]struct{})
	for _, pkg := range packages {
		if pkg.ImportPath == "" || pkg.Standard || !repositoryLocalGoPackage(pkg, context.WorkspaceRoot) {
			continue
		}
		if samePackageTestVariant(pkg, targetImport) || generatedTestDriver(pkg, targetImport) {
			continue
		}
		if _, admitted := allowed[pkg.ImportPath]; !admitted {
			violations[pkg.ImportPath] = struct{}{}
		}
	}
	if len(violations) == 0 {
		return nil
	}
	unexpected := make([]string, 0, len(violations))
	for importPath := range violations {
		unexpected = append(unexpected, importPath)
	}
	sort.Strings(unexpected)
	return fmt.Errorf(
		"performance tests add undeclared repository-local package(s): %s",
		strings.Join(unexpected, ", "),
	)
}

func repositoryLocalGoPackage(pkg goListPackage, workspaceRoot string) bool {
	if pkg.Dir == "" {
		return false
	}
	_, inside := relativeWithin(workspaceRoot, pkg.Dir)
	return inside
}

func samePackageTestVariant(pkg goListPackage, targetImport string) bool {
	return pkg.ForTest == targetImport && strings.HasSuffix(pkg.ImportPath, " ["+targetImport+".test]")
}

func generatedTestDriver(pkg goListPackage, targetImport string) bool {
	return pkg.ForTest == "" && pkg.Name == "main" && pkg.ImportPath == targetImport+".test"
}

func recordPackageModule(
	pkg goListPackage,
	context inventoryContext,
	modules map[string]ModuleIdentity,
) error {
	if pkg.Module == nil {
		return nil
	}
	module := moduleIdentity(pkg.Module, context.WorkspaceRoot)
	modules[moduleIdentityKey(module)] = module
	if pkg.Module.Path != "github.com/windshare/windshare/core" {
		return nil
	}
	effectiveDir, _, _ := effectiveModuleLocation(pkg.Module)
	wanted := filepath.Join(context.WorkspaceRoot, "core")
	if !samePath(effectiveDir, wanted) {
		return fmt.Errorf("root workload resolved core from %s instead of snapshot %s", effectiveDir, wanted)
	}
	return nil
}

func inventoryPackageFiles(
	pkg goListPackage,
	context inventoryContext,
	replacements map[string]overlayReplacement,
	filesByLogical map[string]inventoryFile,
) error {
	includeTests := pkg.ImportPath == context.Overlay.PackageImport || pkg.ForTest == context.Overlay.PackageImport
	for _, name := range pkg.buildFiles(includeTests) {
		physical := name
		if !filepath.IsAbs(physical) {
			physical = filepath.Join(pkg.Dir, filepath.FromSlash(name))
		}
		logicalPhysical := physical
		if derivedCompilerInput(logicalPhysical, context) {
			continue
		}
		originOverride := ""
		if replacement, replaced := replacements[canonicalPath(physical)]; replaced {
			physical = replacement.StubPath
			originOverride = "overlay"
		}
		file, err := inventoryBuildFile(logicalPhysical, physical, pkg, context, originOverride)
		if err != nil {
			return err
		}
		if existing, duplicate := filesByLogical[file.Logical]; duplicate {
			if existing.SHA256 != file.SHA256 || existing.Bytes != file.Bytes {
				return fmt.Errorf("compiled input collision at %s", file.Logical)
			}
			continue
		}
		filesByLogical[file.Logical] = file
	}
	return nil
}

func inventoryBuildFile(
	logicalPhysical string,
	physical string,
	pkg goListPackage,
	context inventoryContext,
	originOverride string,
) (inventoryFile, error) {
	info, err := os.Lstat(physical)
	if err != nil {
		return inventoryFile{}, fmt.Errorf("inspect compiled input %s: %w", physical, err)
	}
	if isReparsePointInfo(info) || !info.Mode().IsRegular() {
		return inventoryFile{}, fmt.Errorf("compiled input %s is not a regular file", physical)
	}
	if info.Size() < 0 || info.Size() > maximumSnapshotSingleFileBytes {
		return inventoryFile{}, fmt.Errorf(
			"compiled input %s exceeds maximum file bytes %d", physical, maximumSnapshotSingleFileBytes,
		)
	}
	sha, err := hashFileExact(physical, info.Size())
	if err != nil {
		return inventoryFile{}, fmt.Errorf("hash compiled input %s: %w", physical, err)
	}
	origin, logical, workspaceRelative, err := logicalInputPath(logicalPhysical, pkg, context)
	if err != nil {
		return inventoryFile{}, err
	}
	if originOverride != "" {
		origin = originOverride
		logical = "overlay/" + context.Overlay.WorkloadID + "/" + filepath.ToSlash(workspaceRelative)
	}
	return inventoryFile{
		Logical: logical, Physical: physical, Origin: origin,
		WorkspaceRelative: workspaceRelative,
		Mode:              info.Mode(), Bytes: info.Size(), SHA256: sha,
	}, nil
}

func logicalInputPath(path string, pkg goListPackage, context inventoryContext) (string, string, string, error) {
	if relative, inside := relativeWithin(context.WorkspaceRoot, path); inside {
		return "workspace", "workspace/" + filepath.ToSlash(relative), filepath.ToSlash(relative), nil
	}
	if relative, inside := relativeWithin(context.GoRoot, path); inside {
		return "toolchain", "toolchain/" + filepath.ToSlash(relative), "", nil
	}
	if pkg.Module != nil {
		if origin, logical, workspaceRelative, found, err := logicalModuleInputPath(path, pkg.Module, context); found || err != nil {
			return origin, logical, workspaceRelative, err
		}
	}
	return "", "", "", fmt.Errorf("compiled input %s is outside workspace, toolchain, and verified module cache", path)
}

func derivedCompilerInput(path string, context inventoryContext) bool {
	for _, generatedRoot := range []string{context.GoCache, context.Temporary} {
		if generatedRoot != "" {
			if _, inside := relativeWithin(generatedRoot, path); inside {
				return true
			}
		}
	}
	return false
}

func logicalModuleInputPath(
	path string,
	module *goListModule,
	context inventoryContext,
) (string, string, string, bool, error) {
	moduleDir, modulePath, moduleVersion := effectiveModuleLocation(module)
	relative, inside := relativeWithin(moduleDir, path)
	if !inside {
		return "", "", "", false, nil
	}
	if _, workspaceLocal := relativeWithin(context.WorkspaceRoot, moduleDir); workspaceLocal {
		workspaceRelative, err := filepath.Rel(context.WorkspaceRoot, path)
		if err != nil {
			return "", "", "", true, err
		}
		workspaceRelative = filepath.ToSlash(workspaceRelative)
		return "workspace", "workspace/" + workspaceRelative, workspaceRelative, true, nil
	}
	if _, cacheLocal := relativeWithin(context.GoModCache, moduleDir); !cacheLocal {
		return "", "", "", true, fmt.Errorf("local module replacement %s escapes the workspace", moduleDir)
	}
	identity := modulePath
	if moduleVersion != "" {
		identity += "@" + moduleVersion
	}
	return "module", "module/" + identity + "/" + filepath.ToSlash(relative), "", true, nil
}

func effectiveModuleLocation(module *goListModule) (string, string, string) {
	directory, path, version := module.Dir, module.Path, module.Version
	if module.Replace == nil {
		return directory, path, version
	}
	if module.Replace.Dir != "" {
		directory = module.Replace.Dir
	}
	if module.Replace.Path != "" {
		path = module.Replace.Path
	}
	return directory, path, module.Replace.Version
}

func materializeWorkspace(
	workspaceRoot string,
	inventories map[string]workloadInventory,
) ([]inventoryFile, error) {
	byDestination := make(map[string]inventoryFile)
	for _, inventory := range inventories {
		for _, file := range inventory.Files {
			if file.Origin != "workspace" && file.Origin != "overlay" {
				continue
			}
			if file.WorkspaceRelative == "" {
				return nil, fmt.Errorf("local compiled input %s has no workspace path", file.Logical)
			}
			destination, err := confinedJoin(workspaceRoot, file.WorkspaceRelative)
			if err != nil {
				return nil, err
			}
			if existing, found := byDestination[destination]; found && existing.SHA256 != file.SHA256 {
				return nil, fmt.Errorf(
					"snapshot destination collision at %s (%s/%s != %s/%s)",
					file.WorkspaceRelative, existing.Origin, existing.SHA256, file.Origin, file.SHA256,
				)
			}
			byDestination[destination] = file
		}
	}
	materialized := make([]inventoryFile, 0, len(byDestination))
	for destination, file := range byDestination {
		origin := file.Origin
		if err := copyExclusiveFileExact(
			file.Physical, destination, file.Mode.Perm(), file.Bytes, file.SHA256,
		); err != nil {
			return nil, fmt.Errorf("materialize %s: %w", file.Logical, err)
		}
		file.Physical = destination
		if origin == "workspace" {
			file.Origin = "workspace"
			file.Logical = "workspace/" + filepath.ToSlash(file.WorkspaceRelative)
			materialized = append(materialized, file)
		}
	}
	return materialized, nil
}

func materializeWorkspaceManifests(repositoryRoot, workspaceRoot string, files *[]inventoryFile) error {
	for _, relative := range []string{"go.work", "go.work.sum", "go.mod", "go.sum", "core/go.mod", "core/go.sum"} {
		source := filepath.Join(repositoryRoot, filepath.FromSlash(relative))
		info, err := os.Lstat(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("inspect workspace manifest %s: %w", relative, err)
		}
		destination, err := confinedJoin(workspaceRoot, relative)
		if err != nil {
			return err
		}
		if _, statErr := os.Stat(destination); errors.Is(statErr, os.ErrNotExist) {
			if err := copyExclusiveFile(source, destination, info.Mode().Perm()); err != nil {
				return fmt.Errorf("copy workspace manifest %s: %w", relative, err)
			}
		}
		sha, err := hashFileExact(destination, info.Size())
		if err != nil {
			return err
		}
		*files = append(*files, inventoryFile{
			Logical: "workspace/" + filepath.ToSlash(relative), Physical: destination,
			Origin: "workspace", WorkspaceRelative: filepath.ToSlash(relative),
			Mode: info.Mode(), Bytes: info.Size(), SHA256: sha,
		})
	}
	return nil
}

func materializeOverlay(
	repositoryRoot string,
	workspaceRoot string,
	snapshotRoot string,
	moduleRoot string,
	preliminary workloadOverlay,
) (workloadOverlay, error) {
	result := preliminary
	result.Replacements = nil
	overlayRoot := filepath.Join(snapshotRoot, "overlays", preliminary.WorkloadID)
	for index, replacement := range preliminary.Replacements {
		relative, inside := relativeWithin(repositoryRoot, replacement.SourcePath)
		if !inside {
			return workloadOverlay{}, fmt.Errorf("overlay source %s escapes repository", replacement.SourcePath)
		}
		source, err := confinedJoin(workspaceRoot, relative)
		if err != nil {
			return workloadOverlay{}, err
		}
		stub := filepath.Join(overlayRoot, "stubs", fmt.Sprintf("%03d-%s", index, filepath.Base(relative)))
		if err := copyExclusiveFile(replacement.StubPath, stub, 0o600); err != nil {
			return workloadOverlay{}, fmt.Errorf("materialize overlay stub: %w", err)
		}
		result.Replacements = append(result.Replacements, overlayReplacement{
			SourcePath: source, StubPath: stub, Package: replacement.Package,
		})
	}
	result.PackageDirectory = filepath.Join(workspaceRoot, strings.TrimPrefix(filepath.ToSlash(preliminary.PackageDirectory), filepath.ToSlash(repositoryRoot)))
	result.OverlayPath = filepath.Join(overlayRoot, "overlay.json")
	digest, err := writeGoOverlayRelativeTo(result.OverlayPath, result.Replacements, moduleRoot)
	if err != nil {
		return workloadOverlay{}, err
	}
	result.OverlayFileSHA256 = digest
	return result, nil
}

func buildGraphIdentity(
	artifactRoot string,
	overlay workloadOverlay,
	inventory workloadInventory,
) (BuildGraphIdentity, error) {
	overlayRelative, inside := relativeWithin(artifactRoot, overlay.OverlayPath)
	if !inside {
		return BuildGraphIdentity{}, errors.New("overlay is outside the evidence artifact root")
	}
	mappings := make([]OverlayMapping, 0, len(overlay.Replacements))
	for _, replacement := range overlay.Replacements {
		sourceRelative, inside := relativeWithin(
			filepath.Join(artifactRoot, snapshotDirectoryName, "workspace"), replacement.SourcePath,
		)
		if !inside {
			return BuildGraphIdentity{}, errors.New("overlay source is outside the snapshot workspace")
		}
		stubRelative, inside := relativeWithin(artifactRoot, replacement.StubPath)
		if !inside {
			return BuildGraphIdentity{}, errors.New("overlay stub is outside the evidence artifact root")
		}
		stubSHA, err := hashFile(replacement.StubPath)
		if err != nil {
			return BuildGraphIdentity{}, err
		}
		mappings = append(mappings, OverlayMapping{
			LogicalPath: filepath.ToSlash(sourceRelative),
			StubPath:    filepath.ToSlash(stubRelative), Package: replacement.Package, StubSHA256: stubSHA,
		})
	}
	sort.Slice(mappings, func(left, right int) bool { return mappings[left].LogicalPath < mappings[right].LogicalPath })
	semanticOverlay, err := semanticOverlaySHA(
		overlay.PackageImport, overlay.PerformanceTests, overlay.SuppressedTests,
		overlay.BenchmarkHarnessPackages, mappings,
	)
	if err != nil {
		return BuildGraphIdentity{}, fmt.Errorf("encode semantic overlay: %w", err)
	}
	return BuildGraphIdentity{
		WorkloadID: overlay.WorkloadID, PackageImportPath: overlay.PackageImport,
		ClosureSHA256: inventory.Closure, OverlaySHA256: semanticOverlay,
		OverlayPath:              filepath.ToSlash(overlayRelative),
		PerformanceTests:         append([]string(nil), overlay.PerformanceTests...),
		SuppressedTests:          append([]string(nil), overlay.SuppressedTests...),
		BenchmarkHarnessPackages: append([]string(nil), overlay.BenchmarkHarnessPackages...),
		OverlayMappings:          mappings, DependencyPackages: append([]string(nil), inventory.Packages...),
	}, nil
}

func semanticOverlaySHA(
	packageImport string,
	performanceTests []string,
	suppressedTests []string,
	benchmarkHarnessPackages []string,
	mappings []OverlayMapping,
) (string, error) {
	return hashJSON(struct {
		PackageImport            string           `json:"packageImport"`
		PerformanceTests         []string         `json:"performanceTests"`
		SuppressedTests          []string         `json:"suppressedTests"`
		BenchmarkHarnessPackages []string         `json:"benchmarkHarnessPackages"`
		Mappings                 []OverlayMapping `json:"mappings"`
	}{packageImport, performanceTests, suppressedTests, benchmarkHarnessPackages, mappings})
}

func verifyDownloadedModules(
	ctx context.Context,
	runner CommandRunner,
	environment controlledGoEnvironment,
	repositoryRoot string,
	workloads []Workload,
) error {
	modules := make(map[string]struct{}, len(workloads))
	for _, workload := range workloads {
		modules[filepath.Clean(filepath.FromSlash(workload.ModuleDir))] = struct{}{}
	}
	moduleDirectories := make([]string, 0, len(modules))
	for module := range modules {
		moduleDirectories = append(moduleDirectories, module)
	}
	sort.Strings(moduleDirectories)
	for _, module := range moduleDirectories {
		directory := filepath.Join(repositoryRoot, filepath.FromSlash(module))
		_, err := runControlled(ctx, runner, Command{
			Executable: environment.GoExecutable,
			Arguments:  []string{"mod", "verify"}, Directory: directory,
			Environment:        environment.withWorkspace(filepath.Join(repositoryRoot, "go.work"), false),
			ReplaceEnvironment: true,
		})
		if err != nil {
			return fmt.Errorf("verify downloaded modules for %s: %w", module, err)
		}
	}
	return nil
}

func verifyDownloadedModulesUnderAuthority(
	ctx context.Context,
	runner CommandRunner,
	environment controlledGoEnvironment,
	repositoryRoot string,
	workloads []Workload,
	authority byteConsumptionAuthority,
) error {
	if authority == nil {
		return errors.New("authoritative module verification requires live byte authority")
	}
	if err := authority.Verify(); err != nil {
		return fmt.Errorf("verify module bytes before final module verification: %w", err)
	}
	verifyErr := verifyDownloadedModules(ctx, runner, environment, repositoryRoot, workloads)
	authorityErr := authority.Verify()
	if verifyErr != nil || authorityErr != nil {
		return errors.Join(
			verifyErr,
			wrapError("verify module bytes after final module verification", authorityErr),
		)
	}
	return nil
}

func wrapError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func moduleIdentity(module *goListModule, workspaceRoot string) ModuleIdentity {
	identity := ModuleIdentity{
		Path: module.Path, Version: module.Version, Sum: module.Sum, GoModSum: module.GoModSum,
	}
	effective := module
	if module.Replace != nil {
		identity.ReplacementPath = module.Replace.Path
		identity.Replacement = module.Replace.Version
		effective = module.Replace
	}
	_, identity.Local = relativeWithin(workspaceRoot, effective.Dir)
	return identity
}

func moduleIdentityKey(module ModuleIdentity) string {
	return strings.Join([]string{
		module.Path, module.Version, module.Sum, module.GoModSum,
		module.ReplacementPath, module.Replacement, fmt.Sprintf("%t", module.Local),
	}, "\x00")
}

func closureSHA(
	files []inventoryFile,
	modules []ModuleIdentity,
	packages []string,
	overlay workloadOverlay,
) (string, error) {
	type closureFile struct {
		Path string `json:"path"`
		SHA  string `json:"sha256"`
	}
	encodedFiles := make([]closureFile, 0, len(files))
	for _, file := range files {
		path := file.Logical
		if file.Origin == "overlay" {
			path = "overlay-target/" + filepath.ToSlash(file.WorkspaceRelative)
		}
		encodedFiles = append(encodedFiles, closureFile{Path: path, SHA: file.SHA256})
	}
	input := struct {
		Files                    []closureFile    `json:"files"`
		Modules                  []ModuleIdentity `json:"modules"`
		Packages                 []string         `json:"packages"`
		Performance              []string         `json:"performanceTests"`
		Suppressed               []string         `json:"suppressedTests"`
		BenchmarkHarnessPackages []string         `json:"benchmarkHarnessPackages"`
	}{
		encodedFiles, modules, packages, overlay.PerformanceTests, overlay.SuppressedTests,
		overlay.BenchmarkHarnessPackages,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode build closure: %w", err)
	}
	return hashBytes(encoded), nil
}

func canonicalSourceFiles(files []inventoryFile) ([]SourceFile, error) {
	byPath := make(map[string]SourceFile)
	for _, file := range files {
		record := SourceFile{
			Path: file.Logical, Origin: file.Origin, Kind: "file", Mode: uint32(file.Mode),
			Bytes: file.Bytes, SHA256: file.SHA256,
		}
		if existing, found := byPath[record.Path]; found && existing != record {
			return nil, fmt.Errorf("source identity collision at %s", record.Path)
		}
		byPath[record.Path] = record
	}
	result := make([]SourceFile, 0, len(byPath))
	for _, file := range byPath {
		result = append(result, file)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Path < result[right].Path })
	return result, nil
}

func classifyCommittedInputs(
	ctx context.Context,
	runner CommandRunner,
	repositoryRoot string,
	commitOID string,
	files []SourceFile,
	workspaceRoot string,
) ([]SourceFile, []string, error) {
	if strings.TrimSpace(commitOID) == "" {
		return nil, nil, errors.New("classify committed inputs requires an immutable commit OID")
	}
	formatResult, err := runGit(ctx, runner, repositoryRoot, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, nil, err
	}
	objectFormat := strings.TrimSpace(string(formatResult))
	treeResult, err := runGit(ctx, runner, repositoryRoot, "ls-tree", "-rz", "--full-tree", commitOID)
	if err != nil {
		return nil, nil, err
	}
	committedObjects, err := parseGitTree(treeResult)
	if err != nil {
		return nil, nil, err
	}
	result := append([]SourceFile(nil), files...)
	var uncommitted []string
	for index := range result {
		if result[index].Origin != "workspace" || !strings.HasPrefix(result[index].Path, "workspace/") {
			continue
		}
		relative := strings.TrimPrefix(result[index].Path, "workspace/")
		path, err := confinedJoin(workspaceRoot, relative)
		if err != nil {
			return nil, nil, err
		}
		objectID, err := gitBlobObjectID(path, result[index].Bytes, objectFormat)
		if err != nil {
			return nil, nil, err
		}
		result[index].Committed = committedObjects[filepath.ToSlash(relative)] == objectID
		if !result[index].Committed {
			uncommitted = append(uncommitted, filepath.ToSlash(relative))
		}
	}
	sort.Strings(uncommitted)
	return result, uncommitted, nil
}

func parseGitTree(encoded []byte) (map[string]string, error) {
	result := make(map[string]string)
	for record := range strings.SplitSeq(string(encoded), "\x00") {
		if record == "" {
			continue
		}
		header, path, found := strings.Cut(record, "\t")
		fields := strings.Fields(header)
		if !found || len(fields) != 3 || fields[1] != "blob" || path == "" {
			continue
		}
		if _, duplicate := result[filepath.ToSlash(path)]; duplicate {
			return nil, fmt.Errorf("git tree repeats %s", path)
		}
		result[filepath.ToSlash(path)] = fields[2]
	}
	return result, nil
}

func gitBlobObjectID(path string, size int64, objectFormat string) (objectID string, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	var digest io.Writer
	var sum func() []byte
	switch objectFormat {
	case "sha1":
		hash := sha1.New() //nolint:gosec // Git object compatibility, never cryptographic trust.
		digest, sum = hash, func() []byte { return hash.Sum(nil) }
	case "sha256":
		hash := sha256.New()
		digest, sum = hash, func() []byte { return hash.Sum(nil) }
	default:
		return "", fmt.Errorf("unsupported Git object format %q", objectFormat)
	}
	if _, err := fmt.Fprintf(digest, "blob %d\x00", size); err != nil {
		return "", err
	}
	if size < 0 || size > maximumSnapshotSingleFileBytes {
		return "", fmt.Errorf("Git blob %s has invalid byte count %d", path, size)
	}
	observed, err := io.Copy(digest, io.LimitReader(file, size+1))
	if err != nil {
		return "", err
	}
	if observed != size {
		return "", fmt.Errorf("Git blob %s changed size while hashing: got %d, want %d", path, observed, size)
	}
	return hex.EncodeToString(sum()), nil
}

type semanticPath struct {
	Physical string
	Logical  string
}

func comparableBuildEnvironment(
	values []EnvironmentVariable,
	paths map[string]semanticPath,
) ([]EnvironmentVariable, error) {
	result := append([]EnvironmentVariable(nil), values...)
	seen := make(map[string]struct{}, len(result))
	for index := range result {
		if _, duplicate := seen[result[index].Name]; duplicate {
			return nil, fmt.Errorf("build environment repeats %s", result[index].Name)
		}
		seen[result[index].Name] = struct{}{}
		semantic, pathValue := paths[result[index].Name]
		if !pathValue {
			continue
		}
		if !samePath(result[index].Value, semantic.Physical) {
			return nil, fmt.Errorf("controlled %s path escaped its semantic location", result[index].Name)
		}
		result[index].Value = semantic.Logical
	}
	for name := range paths {
		if _, found := seen[name]; !found {
			return nil, fmt.Errorf("controlled build environment omitted %s", name)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result, nil
}

func overlayFileDiagnostics(
	artifactRoot string,
	overlays map[string]workloadOverlay,
) ([]OverlayFileDiagnostics, error) {
	result := make([]OverlayFileDiagnostics, 0, len(overlays))
	for workloadID, overlay := range overlays {
		relative, inside := relativeWithin(artifactRoot, overlay.OverlayPath)
		if !inside {
			return nil, fmt.Errorf("workload %s overlay is outside the evidence artifact root", workloadID)
		}
		if overlay.OverlayFileSHA256 == "" {
			return nil, fmt.Errorf("workload %s overlay file identity is missing", workloadID)
		}
		result = append(result, OverlayFileDiagnostics{
			WorkloadID: workloadID,
			Path:       filepath.ToSlash(relative),
			SHA256:     overlay.OverlayFileSHA256,
		})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].WorkloadID < result[right].WorkloadID })
	return result, nil
}

func snapshotIdentitySHA(identity SnapshotIdentity) (string, error) {
	// Runtime locations remain observable, but they cannot redefine the byte
	// identity used to compare two equivalent sealed builds.
	comparable := struct {
		Git                       SourceIdentity        `json:"git"`
		Files                     []SourceFile          `json:"files"`
		ConsumptionInputs         []SourceFile          `json:"consumptionInputs"`
		CompiledInputsMatchCommit bool                  `json:"compiledInputsMatchCommit"`
		UncommittedInputs         []string              `json:"uncommittedInputs,omitempty"`
		BuildEnvironment          []EnvironmentVariable `json:"buildEnvironment"`
		Toolchain                 ToolchainIdentity     `json:"toolchain"`
		Modules                   []ModuleIdentity      `json:"modules"`
		BuildGraphs               []BuildGraphIdentity  `json:"buildGraphs"`
	}{
		Git: identity.Git, Files: identity.Files, ConsumptionInputs: identity.ConsumptionInputs,
		CompiledInputsMatchCommit: identity.CompiledInputsMatchCommit,
		UncommittedInputs:         identity.UncommittedInputs,
		BuildEnvironment:          identity.BuildEnvironment,
		Toolchain:                 identity.Toolchain,
		Modules:                   identity.Modules,
		BuildGraphs:               identity.BuildGraphs,
	}
	return hashJSON(comparable)
}

func hashJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return hashBytes(encoded), nil
}

func sealSnapshotTree(root string) error {
	var directories []string
	err := walkBoundedTree(
		root, maximumSnapshotInputObjects, maximumSnapshotInputDepth,
		func(path, _ string, info os.FileInfo) (bool, error) {
			if isReparsePointInfo(info) {
				return false, fmt.Errorf("snapshot contains reparse point %s", path)
			}
			if info.IsDir() {
				directories = append(directories, path)
				return true, nil
			}
			if !info.Mode().IsRegular() {
				return false, fmt.Errorf("snapshot contains unsupported object %s", path)
			}
			return false, os.Chmod(path, 0o400)
		})
	if err != nil {
		return fmt.Errorf("seal snapshot files: %w", err)
	}
	sort.Slice(directories, func(left, right int) bool { return len(directories[left]) > len(directories[right]) })
	for _, directory := range directories {
		if err := os.Chmod(directory, 0o500); err != nil {
			return fmt.Errorf("seal snapshot directory: %w", err)
		}
	}
	return nil
}

func copyExclusiveFile(source, destination string, mode os.FileMode) (resultErr error) {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || isReparsePointInfo(info) || info.Size() > maximumSnapshotSingleFileBytes {
		return fmt.Errorf("copy source %s is not a bounded real regular file", source)
	}
	digest, err := hashFileExact(source, info.Size())
	if err != nil {
		return err
	}
	return copyExclusiveFileExact(source, destination, mode, info.Size(), digest)
}

func copyExclusiveFileExact(
	source string,
	destination string,
	mode os.FileMode,
	expectedBytes int64,
	expectedSHA256 string,
) (resultErr error) {
	if expectedBytes < 0 || expectedBytes > maximumSnapshotSingleFileBytes {
		return fmt.Errorf(
			"copy source %s byte count %d exceeds maximum %d",
			source, expectedBytes, maximumSnapshotSingleFileBytes,
		)
	}
	pathInfo, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !pathInfo.Mode().IsRegular() || isReparsePointInfo(pathInfo) {
		return fmt.Errorf("copy source %s is not a real regular file", source)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, input.Close()) }()
	openedInfo, err := input.Stat()
	if err != nil {
		return err
	}
	if !openedInfo.Mode().IsRegular() || isReparsePointInfo(openedInfo) ||
		!os.SameFile(pathInfo, openedInfo) || openedInfo.Size() != expectedBytes {
		return fmt.Errorf("copy source %s changed before bounded transfer", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err := copyExactIdentity(
		output, bufio.NewReader(input), source, expectedBytes, expectedSHA256,
	); err != nil {
		return errors.Join(err, output.Close())
	}
	// These copies are still private, transient stage state. Publication owns
	// the later tree-wide durability barrier; syncing every GOROOT source file
	// here turns a bounded generation copy into thousands of serial disk flushes.
	return output.Close()
}

func copyExactIdentity(
	destination io.Writer,
	source io.Reader,
	description string,
	expectedBytes int64,
	expectedSHA256 string,
) error {
	if expectedBytes < 0 || expectedBytes > maximumSnapshotSingleFileBytes {
		return fmt.Errorf(
			"copy source %s byte count %d exceeds maximum %d",
			description, expectedBytes, maximumSnapshotSingleFileBytes,
		)
	}
	digest := sha256.New()
	written, err := io.Copy(
		io.MultiWriter(destination, digest),
		io.LimitReader(source, expectedBytes+1),
	)
	if err != nil {
		return err
	}
	if written != expectedBytes {
		return fmt.Errorf("copy source %s produced %d bytes, expected %d", description, written, expectedBytes)
	}
	if observedSHA256 := hex.EncodeToString(digest.Sum(nil)); observedSHA256 != expectedSHA256 {
		return fmt.Errorf("copy source %s changed during bounded transfer", description)
	}
	return nil
}

func confinedJoin(root, relative string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe snapshot path %q", relative)
	}
	return filepath.Join(root, clean), nil
}

func relativeWithin(root, path string) (string, bool) {
	if root == "" || path == "" {
		return "", false
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", false
	}
	return relative, true
}

func samePath(left, right string) bool {
	leftCanonical := canonicalPath(left)
	rightCanonical := canonicalPath(right)
	if leftCanonical == rightCanonical {
		return true
	}
	return platformPathAlias(leftCanonical, rightCanonical)
}

func canonicalPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return platformPathKey(filepath.Clean(path))
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	}
	return platformPathKey(filepath.Clean(absolute))
}

func processEnvironmentEvidence(environment []string) ([]EnvironmentVariable, error) {
	result := make([]EnvironmentVariable, 0, len(environment))
	seen := make(map[string]struct{}, len(environment))
	for _, assignment := range environment {
		name, value, found := strings.Cut(assignment, "=")
		if !found || name == "" {
			return nil, fmt.Errorf("controlled process environment contains malformed assignment %q", assignment)
		}
		name = strings.ToUpper(name)
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("controlled process environment repeats %s", name)
		}
		seen[name] = struct{}{}
		result = append(result, EnvironmentVariable{Name: name, Value: value})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result, nil
}

func replaceEvidenceEnvironment(values []EnvironmentVariable, name, value string) []EnvironmentVariable {
	result := append([]EnvironmentVariable(nil), values...)
	for index := range result {
		if result[index].Name == name {
			result[index].Value = value
			return result
		}
	}
	return append(result, EnvironmentVariable{Name: name, Value: value})
}
