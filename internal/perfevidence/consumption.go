package perfevidence

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

const consumptionAuthorityDirectoryName = "consumption-authority"

const (
	maximumSnapshotInputObjects    = 250_000
	maximumSnapshotInputBytes      = int64(8 << 30)
	maximumSnapshotInputDepth      = 64
	maximumSnapshotSingleFileBytes = int64(2 << 30)
	maximumDirectoryReadBatch      = 128
)

type snapshotInputBudget struct {
	MaximumObjects   int
	MaximumBytes     int64
	MaximumDepth     int
	MaximumFileBytes int64
}

func defaultSnapshotInputBudget() snapshotInputBudget {
	return snapshotInputBudget{
		MaximumObjects: maximumSnapshotInputObjects, MaximumBytes: maximumSnapshotInputBytes,
		MaximumDepth: maximumSnapshotInputDepth, MaximumFileBytes: maximumSnapshotSingleFileBytes,
	}
}

type snapshotInputMeter struct {
	budget  snapshotInputBudget
	objects int
	bytes   int64
}

func newSnapshotInputMeter(budget snapshotInputBudget) (*snapshotInputMeter, error) {
	if budget.MaximumObjects < 1 || budget.MaximumBytes < 1 || budget.MaximumDepth < 1 ||
		budget.MaximumFileBytes < 1 || budget.MaximumFileBytes > budget.MaximumBytes {
		return nil, errors.New("snapshot input budget is invalid")
	}
	return &snapshotInputMeter{budget: budget}, nil
}

func (meter *snapshotInputMeter) observe(logical string, regular bool, size int64) error {
	depth := 0
	if logical != "." && logical != "" {
		depth = len(strings.Split(filepath.ToSlash(logical), "/"))
	}
	if depth > meter.budget.MaximumDepth {
		return fmt.Errorf("snapshot input %s exceeds maximum depth %d", logical, meter.budget.MaximumDepth)
	}
	if meter.objects >= meter.budget.MaximumObjects {
		return fmt.Errorf("snapshot inputs exceed maximum object count %d", meter.budget.MaximumObjects)
	}
	meter.objects++
	if !regular {
		return nil
	}
	return meter.observeBytes(logical, size)
}

func (meter *snapshotInputMeter) observeBytes(logical string, size int64) error {
	if size < 0 || size > meter.budget.MaximumFileBytes {
		return fmt.Errorf("snapshot input %s exceeds maximum file bytes %d", logical, meter.budget.MaximumFileBytes)
	}
	if meter.bytes > meter.budget.MaximumBytes-size {
		return fmt.Errorf("snapshot inputs exceed maximum total bytes %d", meter.budget.MaximumBytes)
	}
	meter.bytes += size
	return nil
}

type boundedTreeVisitor func(path, relative string, info os.FileInfo) (descend bool, err error)

func walkBoundedTree(
	root string,
	maximumObjects int,
	maximumDepth int,
	visit boundedTreeVisitor,
) error {
	if maximumObjects < 1 || maximumDepth < 1 || visit == nil {
		return errors.New("bounded tree traversal contract is invalid")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	objects := 1
	descend, err := visit(root, ".", rootInfo)
	if err != nil || !descend {
		return err
	}
	if !rootInfo.IsDir() {
		return errors.New("bounded tree root is not a directory")
	}
	var walkDirectory func(string, string) error
	walkDirectory = func(directoryPath, directoryRelative string) (resultErr error) {
		directory, err := os.Open(directoryPath)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
		for {
			remaining := maximumObjects - objects
			readLimit := maximumDirectoryReadBatch
			if remaining < readLimit {
				readLimit = remaining + 1
			}
			entries, readErr := directory.ReadDir(readLimit)
			for _, entry := range entries {
				if objects >= maximumObjects {
					return fmt.Errorf("tree exceeds maximum object count %d", maximumObjects)
				}
				relative := entry.Name()
				if directoryRelative != "." {
					relative = filepath.Join(directoryRelative, entry.Name())
				}
				depth := len(strings.Split(filepath.ToSlash(relative), "/"))
				if depth > maximumDepth {
					return fmt.Errorf("tree entry %s exceeds maximum depth %d", relative, maximumDepth)
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				objects++
				path := filepath.Join(directoryPath, entry.Name())
				descend, err := visit(path, relative, info)
				if err != nil {
					return err
				}
				if info.IsDir() && descend {
					if err := walkDirectory(path, relative); err != nil {
						return err
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	return walkDirectory(root, ".")
}

// byteConsumptionAuthority makes a byte identity a live runtime invariant.
// Hashes describe evidence, but only an OS authority can make an ABA mutation
// observable while another process is consuming the bytes.
type byteConsumptionAuthority interface {
	Verify() error
	VerifyProcessStart(evidence protocol.StartEvidence, executable string) (bool, error)
	Close() error
}

type combinedConsumptionAuthority struct {
	authorities []byteConsumptionAuthority
}

func combineConsumptionAuthorities(authorities ...byteConsumptionAuthority) byteConsumptionAuthority {
	filtered := make([]byteConsumptionAuthority, 0, len(authorities))
	for _, authority := range authorities {
		if authority != nil {
			filtered = append(filtered, authority)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return &combinedConsumptionAuthority{authorities: filtered}
}

func (authority *combinedConsumptionAuthority) Verify() error {
	return verifyConsumptionAuthorities(authority.authorities)
}

func (authority *combinedConsumptionAuthority) VerifyProcessStart(
	evidence protocol.StartEvidence,
	executable string,
) (bool, error) {
	matched := false
	var errs []error
	for _, member := range authority.authorities {
		protected, err := member.VerifyProcessStart(evidence, executable)
		matched = matched || protected
		errs = append(errs, err)
	}
	return matched, errors.Join(errs...)
}

func (authority *combinedConsumptionAuthority) Close() error {
	var errs []error
	for _, member := range authority.authorities {
		errs = append(errs, member.Close())
	}
	authority.authorities = nil
	return errors.Join(errs...)
}

func inventoryConsumptionUniverse(roots map[string]string) ([]inventoryFile, error) {
	return inventoryConsumptionUniverseWithBudget(roots, defaultSnapshotInputBudget())
}

func inventoryConsumptionUniverseWithBudget(
	roots map[string]string,
	budget snapshotInputBudget,
) ([]inventoryFile, error) {
	meter, err := newSnapshotInputMeter(budget)
	if err != nil {
		return nil, err
	}
	var files []inventoryFile
	rootNames := make([]string, 0, len(roots))
	for name := range roots {
		rootNames = append(rootNames, name)
	}
	sort.Strings(rootNames)
	for _, name := range rootNames {
		root := roots[name]
		remainingObjects := budget.MaximumObjects - meter.objects
		if remainingObjects < 1 {
			return nil, fmt.Errorf("snapshot inputs exceed maximum object count %d", budget.MaximumObjects)
		}
		err := walkBoundedTree(
			root, remainingObjects, budget.MaximumDepth,
			func(path, relative string, info os.FileInfo) (bool, error) {
				if meter.objects >= budget.MaximumObjects {
					return false, fmt.Errorf("snapshot inputs exceed maximum object count %d", budget.MaximumObjects)
				}
				meter.objects++
				if isReparsePointInfo(info) {
					return false, fmt.Errorf("consumption universe contains reparse point %s", path)
				}
				if info.Mode().IsRegular() {
					logical := filepath.Join(name, relative)
					if err := meter.observeBytes(logical, info.Size()); err != nil {
						return false, err
					}
				}
				if info.IsDir() {
					return true, nil
				}
				if !info.Mode().IsRegular() {
					return false, fmt.Errorf("consumption universe contains unsupported object %s", path)
				}
				sha, err := hashFileExact(path, info.Size())
				if err != nil {
					return false, err
				}
				logical := "consumption/" + name + "/" + filepath.ToSlash(relative)
				files = append(files, inventoryFile{
					Logical: logical, Physical: path, Origin: "consumption-" + name,
					Mode: info.Mode(), Bytes: info.Size(), SHA256: sha,
				})
				return false, nil
			})
		if err != nil {
			return nil, fmt.Errorf("inventory %s consumption universe: %w", name, err)
		}
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Logical < files[right].Logical })
	return files, nil
}

func verifyConsumptionAuthorities(authorities []byteConsumptionAuthority) error {
	var errs []error
	for _, authority := range authorities {
		if authority != nil {
			errs = append(errs, authority.Verify())
		}
	}
	return errors.Join(errs...)
}

func closeConsumptionAuthority(authority byteConsumptionAuthority) error {
	if authority == nil {
		return nil
	}
	return authority.Close()
}

func isolateControlledGoEnvironment(
	environment controlledGoEnvironment,
	runtimeRoot string,
) (controlledGoEnvironment, error) {
	originalGoRoot := environment.ToolchainLocations.GoRoot
	goExecutableRelative, executableInside := relativeWithin(originalGoRoot, environment.GoExecutable)
	goToolDirRelative, toolDirInside := relativeWithin(originalGoRoot, environment.ToolchainLocations.GoToolDir)
	if !executableInside || !toolDirInside {
		return controlledGoEnvironment{}, errors.New("Go executable and tool directory must be contained by GOROOT")
	}

	authorityRoot := filepath.Join(runtimeRoot, consumptionAuthorityDirectoryName)
	goRoot := filepath.Join(authorityRoot, "goroot")
	goModCache := filepath.Join(authorityRoot, "gomodcache")
	if err := os.MkdirAll(authorityRoot, 0o700); err != nil {
		return controlledGoEnvironment{}, fmt.Errorf("create consumption authority root: %w", err)
	}
	if !samePath(originalGoRoot, goRoot) {
		return controlledGoEnvironment{}, errors.New("controlled Go toolchain was not materialized into its authority root")
	}
	if err := os.Rename(environment.GoModCache, goModCache); err != nil {
		return controlledGoEnvironment{}, fmt.Errorf("isolate verified module cache: %w", err)
	}

	environment.GoExecutable = filepath.Join(goRoot, goExecutableRelative)
	environment.GoModCache = goModCache
	environment.ToolchainLocations = ToolchainDiagnostics{
		ExecutablePath: environment.GoExecutable,
		GoRoot:         goRoot,
		GoToolDir:      filepath.Join(goRoot, goToolDirRelative),
	}
	for name, value := range map[string]string{"GOROOT": goRoot, "GOMODCACHE": goModCache} {
		environment.Offline = replaceEnvironmentValue(environment.Offline, name, value)
		environment.Prefetch = replaceEnvironmentValue(environment.Prefetch, name, value)
		environment.Evidence = replaceEvidenceEnvironment(environment.Evidence, name, value)
	}
	environment.AuthorityRoots = []string{goRoot, goModCache}
	return environment, nil
}

func materializeGoToolchainGeneration(
	sourceExecutable string,
	runtimeRoot string,
) (destinationExecutable string, destinationRoot string, resultAuthority byteConsumptionAuthority, resultErr error) {
	resolvedExecutable, err := filepath.EvalSymlinks(sourceExecutable)
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve Go executable generation: %w", err)
	}
	resolvedExecutable, err = filepath.Abs(resolvedExecutable)
	if err != nil {
		return "", "", nil, err
	}
	binDirectory := filepath.Dir(resolvedExecutable)
	if !strings.EqualFold(filepath.Base(binDirectory), "bin") {
		return "", "", nil, fmt.Errorf("Go executable %s is not inside a GOROOT bin directory", resolvedExecutable)
	}
	sourceRoot := filepath.Dir(binDirectory)
	executableRelative, inside := relativeWithin(sourceRoot, resolvedExecutable)
	if !inside || executableRelative == "." {
		return "", "", nil, errors.New("Go executable escaped its derived GOROOT generation")
	}
	sourceFiles, err := inventoryConsumptionUniverse(map[string]string{"bootstrap-goroot": sourceRoot})
	if err != nil {
		return "", "", nil, err
	}
	sourceAuthority, err := acquireConsumptionAuthority(inventoryValidationTargets(sourceFiles), []string{sourceRoot})
	if err != nil {
		return "", "", nil, fmt.Errorf("retain source Go toolchain generation: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeConsumptionAuthority(sourceAuthority))
		if resultErr != nil {
			resultErr = errors.Join(resultErr, closeConsumptionAuthority(resultAuthority))
			resultAuthority = nil
		}
	}()
	destinationRoot = filepath.Join(runtimeRoot, consumptionAuthorityDirectoryName, "goroot")
	if err := os.MkdirAll(filepath.Dir(destinationRoot), 0o700); err != nil {
		return "", "", nil, err
	}
	if err := copyAuthorityInventory(sourceRoot, destinationRoot, sourceFiles); err != nil {
		return "", "", nil, fmt.Errorf("materialize retained Go toolchain generation: %w", err)
	}
	if err := sourceAuthority.Verify(); err != nil {
		return "", "", nil, fmt.Errorf("source Go toolchain changed while materializing: %w", err)
	}
	destinationFiles, err := inventoryConsumptionUniverse(map[string]string{"goroot": destinationRoot})
	if err != nil {
		return "", "", nil, err
	}
	resultAuthority, err = acquireConsumptionAuthority(
		inventoryValidationTargets(destinationFiles), []string{destinationRoot},
	)
	if err != nil {
		return "", "", nil, fmt.Errorf("retain materialized Go toolchain generation: %w", err)
	}
	if err := sourceAuthority.Verify(); err != nil {
		return "", "", nil, fmt.Errorf("source Go toolchain changed before generation transfer: %w", err)
	}
	destinationExecutable = filepath.Join(destinationRoot, executableRelative)
	return destinationExecutable, destinationRoot, resultAuthority, nil
}

func copyAuthorityInventory(source, destination string, files []inventoryFile) (resultErr error) {
	sourceInfo, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !sourceInfo.IsDir() || isReparsePointInfo(sourceInfo) {
		return fmt.Errorf("authority source %s is not a real directory", source)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	for _, file := range files {
		relative, inside := relativeWithin(source, file.Physical)
		if !inside || relative == "." {
			return fmt.Errorf("authority input %s escaped source root", file.Physical)
		}
		target := filepath.Join(destination, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := copyExclusiveFileExact(
			file.Physical, target, file.Mode.Perm(), file.Bytes, file.SHA256,
		); err != nil {
			return err
		}
	}
	return nil
}

func artifactValidationTarget(stageRoot string, artifact ArtifactFile) (snapshotValidationTarget, error) {
	path, err := confinedJoin(stageRoot, artifact.Path)
	if err != nil {
		return snapshotValidationTarget{}, err
	}
	if artifact.Path == "" || artifact.Bytes <= 0 || len(artifact.SHA256) != 64 {
		return snapshotValidationTarget{}, fmt.Errorf("artifact %s has no byte identity", artifact.Path)
	}
	return snapshotValidationTarget{
		LogicalPath: artifact.Path, PhysicalPath: path, Bytes: artifact.Bytes, SHA256: artifact.SHA256,
	}, nil
}
