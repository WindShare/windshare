package perfevidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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

func publicationPaths(outputRoot, stage string) (rootResult, stageResult string, resultErr error) {
	authority, err := openOutputRootAuthority(outputRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve evidence output root: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, authority.close()) }()
	stagePath, err := filepath.Abs(stage)
	if err != nil {
		return "", "", fmt.Errorf("resolve evidence stage: %w", err)
	}
	if !samePath(filepath.Dir(stagePath), authority.path) {
		return "", "", errors.New("evidence stage must be a direct staging child of its output root")
	}
	info, err := os.Lstat(stagePath)
	if err != nil {
		return "", "", err
	}
	if !strings.HasPrefix(filepath.Base(stagePath), ".staging-") || isReparsePointInfo(info) || !info.IsDir() {
		return "", "", errors.New("evidence stage must be a real direct staging child of its output root")
	}
	return authority.path, stagePath, nil
}

func publicationPathsWithAuthority(
	authority *outputRootAuthority,
	artifactDir *stageDirectoryAuthority,
) (string, string, error) {
	if authority == nil {
		return "", "", errors.New("evidence output authority is nil")
	}
	if err := authority.verifyPath(); err != nil {
		return "", "", err
	}
	if artifactDir == nil {
		return "", "", errors.New("evidence stage authority is nil")
	}
	artifactName := artifactDir.name
	if !strings.HasPrefix(artifactName, ".staging-") || filepath.Base(artifactName) != artifactName {
		return "", "", errors.New("evidence stage must be a direct staging child of its output root")
	}
	if err := artifactDir.verifyName(authority); err != nil {
		return "", "", err
	}
	stagePath := artifactDir.path
	info, err := os.Lstat(stagePath)
	if err != nil {
		return "", "", fmt.Errorf("inspect evidence stage: %w", err)
	}
	if isReparsePointInfo(info) || !info.IsDir() {
		return "", "", errors.New("evidence stage must be a real directory")
	}
	return authority.path, stagePath, nil
}

func resolveDirectoryAuthority(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return filepath.Clean(resolved), nil
}

func validRunID(runID string) bool {
	if len(runID) == 0 || len(runID) > 64 {
		return false
	}
	for _, character := range runID {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' {
			continue
		}
		return false
	}
	return true
}

func recoverAbandonedStages(root string, now time.Time) error {
	return recoverAbandonedStagesWithBudget(root, now, DefaultEvidenceStoreBudget())
}

func recoverAbandonedStagesWithBudget(root string, now time.Time, budget EvidenceStoreBudget) error {
	authority, err := openOutputRootAuthority(root)
	if err != nil {
		return err
	}
	recoveryErr := recoverAbandonedStagesWithAuthorityAndBudget(authority, now, budget)
	return errors.Join(recoveryErr, authority.close())
}

func recoverAbandonedStagesWithAuthority(authority *outputRootAuthority, now time.Time) error {
	return recoverAbandonedStagesWithAuthorityAndBudget(authority, now, DefaultEvidenceStoreBudget())
}

func recoverAbandonedStagesWithAuthorityAndBudget(
	authority *outputRootAuthority,
	now time.Time,
	budget EvidenceStoreBudget,
) error {
	entries, err := authority.readDir()
	if err != nil {
		return fmt.Errorf("scan evidence stages: %w", err)
	}
	if err := preflightEvidenceRecovery(authority, entries, budget); err != nil {
		return fmt.Errorf("preflight evidence recovery: %w", err)
	}
	for _, entry := range entries {
		if err := recoverRuntimeStage(authority, entry.Name(), now, budget); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		if err := recoverOrphanArtifactStage(authority, entry.Name(), now); err != nil {
			return err
		}
	}
	return nil
}

func preflightEvidenceRecovery(
	authority *outputRootAuthority,
	entries []os.DirEntry,
	budget EvidenceStoreBudget,
) error {
	meter, err := newEvidenceStoreMeter(budget)
	if err != nil {
		return err
	}
	if err := meter.observeRootEntries(len(entries)); err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		owned := strings.HasPrefix(name, ".runtime-") || strings.HasPrefix(name, ".staging-")
		if !owned {
			continue
		}
		directory, err := authority.openRecoveryChildAuthority(name)
		if authorityChildAbsent(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("retain evidence recovery candidate %s: %w", name, err)
		}
		walkErr := directory.walkEvidenceStore(&evidenceStoreWalk{meter: meter})
		if err := errors.Join(walkErr, directory.close()); err != nil {
			return fmt.Errorf("inventory evidence recovery candidate %s: %w", name, err)
		}
	}
	return nil
}

func recoverRuntimeStage(
	authority *outputRootAuthority,
	name string,
	now time.Time,
	budget EvidenceStoreBudget,
) (resultErr error) {
	if !strings.HasPrefix(name, ".runtime-") {
		return nil
	}
	runtimeDir, err := authority.openRecoveryChildAuthority(name)
	if err != nil {
		return fmt.Errorf("retain evidence runtime %s: %w", name, err)
	}
	defer func() { resultErr = errors.Join(resultErr, runtimeDir.close()) }()
	leased, err := runtimeDir.tryAcquireRecoveryLease(authority)
	if err != nil {
		return fmt.Errorf("lease evidence runtime %s: %w", name, err)
	}
	if !leased {
		return nil
	}
	modifiedAt, err := runtimeDir.modTime()
	if err != nil {
		return fmt.Errorf("inspect evidence runtime %s: %w", name, err)
	}
	runID := strings.TrimPrefix(name, ".runtime-")
	owner, validOwner, err := readStageOwnerAuthority(runtimeDir, runID, budget)
	if err != nil {
		return fmt.Errorf("read evidence runtime owner %s: %w", name, err)
	}
	if validOwner {
		matches, matchErr := processMatches(owner.ProcessID, owner.ProcessToken)
		if matchErr != nil || matches {
			// An unprovable owner is retained: cleanup must never trade disk
			// reclamation for deletion of another process's live stage.
			return nil
		}
	} else if now.Sub(modifiedAt) < abandonedStageMinimumAge {
		return nil
	}
	artifactName := ".staging-" + runID
	artifactDir, err := authority.openRecoveryChildAuthority(artifactName)
	if err == nil {
		defer func() { resultErr = errors.Join(resultErr, artifactDir.close()) }()
		artifactLeased, leaseErr := artifactDir.tryAcquireRecoveryLease(authority)
		if leaseErr != nil {
			return fmt.Errorf("lease evidence artifact %s: %w", artifactName, leaseErr)
		}
		if !artifactLeased {
			// The owner pathname can be forged or swapped. The artifact's own
			// kernel lease is the deletion authority for a live run.
			return nil
		}
		if err := authority.removeRetainedChild(artifactDir, nil); err != nil {
			return fmt.Errorf("recover abandoned artifact stage %s: %w", runID, err)
		}
	} else if !authorityChildAbsent(err) {
		return fmt.Errorf("retain evidence artifact %s: %w", artifactName, err)
	}
	if err := authority.removeRetainedChild(runtimeDir, nil); err != nil {
		return fmt.Errorf("recover abandoned runtime stage %s: %w", runID, err)
	}
	return nil
}

func readStageOwnerAuthority(
	runtimeDir *stageDirectoryAuthority,
	runID string,
	budget EvidenceStoreBudget,
) (stageOwner, bool, error) {
	meter, meterErr := newEvidenceStoreMeter(budget)
	if meterErr != nil {
		return stageOwner{}, false, meterErr
	}
	encoded, err := readAuthorityFileWithMeter(runtimeDir, stageOwnerName, evidenceMetadataFile, meter)
	if err != nil {
		if authorityChildAbsent(err) {
			return stageOwner{}, false, nil
		}
		return stageOwner{}, false, err
	}
	var owner stageOwner
	if json.Unmarshal(encoded, &owner) != nil {
		return stageOwner{}, false, nil
	}
	valid := owner.SchemaVersion == SchemaVersion && owner.RunID == runID && validRunID(owner.RunID) &&
		owner.ProcessID > 0 && owner.ProcessToken != "" && !owner.CreatedAt.IsZero()
	return owner, valid, nil
}

func recoverOrphanArtifactStage(
	authority *outputRootAuthority,
	name string,
	now time.Time,
) (resultErr error) {
	if !strings.HasPrefix(name, ".staging-") {
		return nil
	}
	artifactDir, err := authority.openRecoveryChildAuthority(name)
	if err != nil {
		if authorityChildAbsent(err) {
			return nil
		}
		return fmt.Errorf("retain orphan evidence stage %s: %w", name, err)
	}
	defer func() { resultErr = errors.Join(resultErr, artifactDir.close()) }()
	leased, err := artifactDir.tryAcquireRecoveryLease(authority)
	if err != nil {
		return fmt.Errorf("lease orphan evidence stage %s: %w", name, err)
	}
	if !leased {
		return nil
	}
	runID := strings.TrimPrefix(name, ".staging-")
	runtimeDir, err := authority.openRecoveryChildAuthority(".runtime-" + runID)
	if err == nil {
		return runtimeDir.close()
	}
	if !authorityChildAbsent(err) {
		return fmt.Errorf("retain orphan stage owner for %s: %w", runID, err)
	}
	modifiedAt, err := artifactDir.modTime()
	if err != nil {
		return fmt.Errorf("inspect orphan evidence stage %s: %w", name, err)
	}
	if now.Sub(modifiedAt) < abandonedStageMinimumAge {
		return nil
	}
	if err := authority.removeRetainedChild(artifactDir, nil); err != nil {
		return fmt.Errorf("recover orphan stage %s: %w", runID, err)
	}
	return nil
}

func removeOwnedTree(root, path string) error {
	authority, err := openOutputRootAuthority(root)
	if err != nil {
		return err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return errors.Join(err, authority.close())
	}
	if !samePath(filepath.Dir(path), authority.path) {
		return errors.Join(
			fmt.Errorf("refusing to remove unowned performance path %s", path), authority.close(),
		)
	}
	removeErr := removeOwnedTreeAuthority(authority, filepath.Base(path), nil)
	return errors.Join(removeErr, authority.close())
}

func removeOwnedTreeAuthority(
	authority *outputRootAuthority,
	name string,
	transition func(string) error,
) error {
	if authority == nil {
		return errors.New("evidence output authority is nil")
	}
	owned := strings.HasPrefix(name, ".staging-") || strings.HasPrefix(name, ".runtime-")
	if filepath.Base(name) != name || !owned {
		return fmt.Errorf("refusing to remove unowned performance child %s", name)
	}
	return authority.removeChild(name, transition)
}
