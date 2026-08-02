package perfevidence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type snapshotRevalidator interface {
	Revalidate() error
}

type snapshotRevalidatorFunc func() error

func (function snapshotRevalidatorFunc) Revalidate() error {
	return function()
}

type snapshotValidationTarget struct {
	LogicalPath  string
	PhysicalPath string
	Bytes        int64
	SHA256       string
}

type snapshotValidationPlan struct {
	ArtifactRoot string
	Identity     SnapshotIdentity
	Targets      []snapshotValidationTarget
}

func (snapshot PreparedSnapshot) Revalidate() error {
	if snapshot.revalidator == nil {
		return errors.New("snapshot has no final-byte revalidator")
	}
	return errors.Join(
		verifySnapshotAuthority(snapshot.authority),
		snapshot.revalidator.Revalidate(),
		verifySnapshotAuthority(snapshot.authority),
	)
}

func (snapshot *PreparedSnapshot) Close() error {
	if snapshot == nil {
		return nil
	}
	authority := snapshot.authority
	domain := snapshot.domain
	snapshot.authority = nil
	snapshot.domain = nil
	snapshot.Environment.Authority = nil
	var domainErr error
	if domain != nil {
		domainErr = domain.Close()
	}
	return errors.Join(domainErr, closeConsumptionAuthority(authority))
}

func verifySnapshotAuthority(authority byteConsumptionAuthority) error {
	if authority == nil {
		return errors.New("snapshot has no live consumption authority")
	}
	return authority.Verify()
}

func newSnapshotValidationPlan(
	artifactRoot string,
	inputs []inventoryFile,
	identity SnapshotIdentity,
	environment controlledGoEnvironment,
) (snapshotValidationPlan, error) {
	byPhysicalPath := make(map[string]snapshotValidationTarget, len(inputs)+len(identity.Diagnostics.OverlayFiles)+1)
	logicalInputs := make(map[string]SourceFile, len(inputs))
	for _, input := range inputs {
		target := snapshotValidationTarget{
			LogicalPath:  input.Logical,
			PhysicalPath: input.Physical,
			Bytes:        input.Bytes,
			SHA256:       input.SHA256,
		}
		if err := addSnapshotValidationTarget(byPhysicalPath, target); err != nil {
			return snapshotValidationPlan{}, err
		}
		logicalInputs[input.Logical] = SourceFile{
			Path: input.Logical, Origin: input.Origin, Kind: "file", Mode: uint32(input.Mode),
			Bytes: input.Bytes, SHA256: input.SHA256,
		}
	}
	recordedInputs := append(append([]SourceFile(nil), identity.Files...), identity.ConsumptionInputs...)
	for _, recorded := range recordedInputs {
		input, found := logicalInputs[recorded.Path]
		if !found || input.Path != recorded.Path || input.Origin != recorded.Origin ||
			input.Bytes != recorded.Bytes || input.SHA256 != recorded.SHA256 {
			return snapshotValidationPlan{}, fmt.Errorf("recorded source %s has no validation target", recorded.Path)
		}
	}
	if err := addOverlayValidationTargets(byPhysicalPath, artifactRoot, identity.Diagnostics.OverlayFiles); err != nil {
		return snapshotValidationPlan{}, err
	}
	if err := addToolchainValidationTargets(byPhysicalPath, identity.Toolchain, environment.ToolchainLocations); err != nil {
		return snapshotValidationPlan{}, err
	}
	targets := make([]snapshotValidationTarget, 0, len(byPhysicalPath))
	for _, target := range byPhysicalPath {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(left, right int) bool {
		return targets[left].LogicalPath < targets[right].LogicalPath
	})
	return snapshotValidationPlan{ArtifactRoot: artifactRoot, Identity: identity, Targets: targets}, nil
}

func addOverlayValidationTargets(
	targets map[string]snapshotValidationTarget,
	artifactRoot string,
	overlays []OverlayFileDiagnostics,
) error {
	for _, overlay := range overlays {
		path, err := confinedJoin(artifactRoot, overlay.Path)
		if err != nil {
			return fmt.Errorf("resolve workload %s overlay: %w", overlay.WorkloadID, err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect workload %s overlay: %w", overlay.WorkloadID, err)
		}
		if isReparsePointInfo(info) || !info.Mode().IsRegular() {
			return fmt.Errorf("inspect workload %s overlay: not a regular file", overlay.WorkloadID)
		}
		if err := addSnapshotValidationTarget(targets, snapshotValidationTarget{
			LogicalPath:  "$OVERLAY/" + overlay.WorkloadID,
			PhysicalPath: path,
			Bytes:        info.Size(),
			SHA256:       overlay.SHA256,
		}); err != nil {
			return err
		}
	}
	return nil
}

func addToolchainValidationTargets(
	targets map[string]snapshotValidationTarget,
	identity ToolchainIdentity,
	locations ToolchainDiagnostics,
) error {
	toolchainInfo, err := os.Lstat(locations.ExecutablePath)
	if err != nil {
		return fmt.Errorf("inspect Go toolchain executable: %w", err)
	}
	if isReparsePointInfo(toolchainInfo) || !toolchainInfo.Mode().IsRegular() {
		return errors.New("go toolchain executable is not a regular file")
	}
	if err := addSnapshotValidationTarget(targets, snapshotValidationTarget{
		LogicalPath:  "$TOOLCHAIN/go",
		PhysicalPath: locations.ExecutablePath,
		Bytes:        toolchainInfo.Size(),
		SHA256:       identity.ExecutableSHA256,
	}); err != nil {
		return err
	}
	for _, tool := range identity.Tools {
		toolPath, err := confinedJoin(locations.GoToolDir, tool.Name)
		if err != nil {
			return fmt.Errorf("resolve Go tool %s: %w", tool.Name, err)
		}
		if err := addSnapshotValidationTarget(targets, snapshotValidationTarget{
			LogicalPath:  "$TOOLCHAIN/tools/" + tool.Name,
			PhysicalPath: toolPath,
			Bytes:        tool.Bytes,
			SHA256:       tool.SHA256,
		}); err != nil {
			return err
		}
	}
	for _, input := range identity.BuildInputs {
		inputPath, err := toolchainInputPath(input, locations)
		if err != nil {
			return err
		}
		if err := addSnapshotValidationTarget(targets, snapshotValidationTarget{
			LogicalPath: fmt.Sprintf("$TOOLCHAIN/%s/%s", input.Root, filepath.ToSlash(input.Path)), PhysicalPath: inputPath,
			Bytes: input.Bytes, SHA256: input.SHA256,
		}); err != nil {
			return err
		}
	}
	return nil
}

func toolchainInputPath(input ToolchainInputIdentity, locations ToolchainDiagnostics) (string, error) {
	var root string
	switch input.Root {
	case ToolchainInputGoRoot:
		root = locations.GoRoot
	case ToolchainInputGoToolDir:
		root = locations.GoToolDir
	default:
		return "", fmt.Errorf("go toolchain build input %s has unsupported root %q", input.Path, input.Root)
	}
	path, err := confinedJoin(root, input.Path)
	if err != nil {
		return "", fmt.Errorf("resolve Go toolchain build input %s: %w", input.Path, err)
	}
	return path, nil
}

func addSnapshotValidationTarget(
	targets map[string]snapshotValidationTarget,
	target snapshotValidationTarget,
) error {
	if target.PhysicalPath == "" || target.SHA256 == "" {
		return fmt.Errorf("validation target %s is incomplete", target.LogicalPath)
	}
	key := canonicalPath(target.PhysicalPath)
	if existing, found := targets[key]; found {
		if existing.Bytes != target.Bytes || existing.SHA256 != target.SHA256 {
			return fmt.Errorf("validation target collision at %s", target.PhysicalPath)
		}
		return nil
	}
	targets[key] = target
	return nil
}

func (plan snapshotValidationPlan) Revalidate() error {
	if plan.ArtifactRoot == "" || len(plan.Targets) == 0 {
		return errors.New("snapshot has no final-byte validation plan")
	}
	if err := revalidateTargets(plan.Targets); err != nil {
		return err
	}
	if err := revalidateSemanticOverlays(plan.ArtifactRoot, plan.Identity.BuildGraphs); err != nil {
		return err
	}
	identitySHA, err := snapshotIdentitySHA(plan.Identity)
	if err != nil {
		return fmt.Errorf("revalidate source identity: %w", err)
	}
	if identitySHA != plan.Identity.SHA256 {
		return errors.New("revalidate source identity: comparable identity changed")
	}
	return nil
}

func revalidateTargets(targets []snapshotValidationTarget) error {
	for _, target := range targets {
		info, err := os.Lstat(target.PhysicalPath)
		if err != nil {
			return fmt.Errorf("revalidate %s: %w", target.LogicalPath, err)
		}
		if isReparsePointInfo(info) || !info.Mode().IsRegular() || info.Size() != target.Bytes {
			return fmt.Errorf("revalidate %s: file type or size changed", target.LogicalPath)
		}
		sha, err := hashFileExact(target.PhysicalPath, target.Bytes)
		if err != nil {
			return fmt.Errorf("revalidate %s: %w", target.LogicalPath, err)
		}
		if sha != target.SHA256 {
			return fmt.Errorf("revalidate %s: content changed", target.LogicalPath)
		}
	}
	return nil
}

func revalidateSemanticOverlays(artifactRoot string, graphs []BuildGraphIdentity) error {
	for _, graph := range graphs {
		mappings := append([]OverlayMapping(nil), graph.OverlayMappings...)
		for index := range mappings {
			stub, err := confinedJoin(artifactRoot, mappings[index].StubPath)
			if err != nil {
				return fmt.Errorf("revalidate workload %s overlay stub: %w", graph.WorkloadID, err)
			}
			stubInfo, statErr := os.Lstat(stub)
			if statErr != nil {
				return fmt.Errorf("revalidate workload %s overlay stub: %w", graph.WorkloadID, statErr)
			}
			if isReparsePointInfo(stubInfo) || !stubInfo.Mode().IsRegular() {
				return fmt.Errorf("revalidate workload %s overlay stub: not a regular file", graph.WorkloadID)
			}
			mappings[index].StubSHA256, err = hashFileExact(stub, stubInfo.Size())
			if err != nil {
				return fmt.Errorf("revalidate workload %s overlay stub: %w", graph.WorkloadID, err)
			}
		}
		semanticSHA, err := semanticOverlaySHA(
			graph.PackageImportPath, graph.PerformanceTests, graph.SuppressedTests,
			graph.BenchmarkHarnessPackages, mappings,
		)
		if err != nil {
			return fmt.Errorf("revalidate workload %s semantic overlay: %w", graph.WorkloadID, err)
		}
		if semanticSHA != graph.OverlaySHA256 {
			return fmt.Errorf("revalidate workload %s semantic overlay: identity changed", graph.WorkloadID)
		}
	}
	return nil
}
