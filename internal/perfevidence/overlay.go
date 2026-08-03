package perfevidence

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const performanceTestSuffix = "_perfevidence_test.go"

type goListModule struct {
	Path     string        `json:"Path"`
	Version  string        `json:"Version"`
	Sum      string        `json:"Sum"`
	GoModSum string        `json:"GoModSum"`
	Dir      string        `json:"Dir"`
	GoMod    string        `json:"GoMod"`
	Main     bool          `json:"Main"`
	Replace  *goListModule `json:"Replace"`
}
type goListPackage struct {
	Dir             string        `json:"Dir"`
	ImportPath      string        `json:"ImportPath"`
	Name            string        `json:"Name"`
	ForTest         string        `json:"ForTest"`
	Standard        bool          `json:"Standard"`
	Module          *goListModule `json:"Module"`
	Deps            []string      `json:"Deps"`
	GoFiles         []string      `json:"GoFiles"`
	CgoFiles        []string      `json:"CgoFiles"`
	CFiles          []string      `json:"CFiles"`
	CXXFiles        []string      `json:"CXXFiles"`
	MFiles          []string      `json:"MFiles"`
	HFiles          []string      `json:"HFiles"`
	FFiles          []string      `json:"FFiles"`
	SFiles          []string      `json:"SFiles"`
	SwigFiles       []string      `json:"SwigFiles"`
	SwigCXXFiles    []string      `json:"SwigCXXFiles"`
	SysoFiles       []string      `json:"SysoFiles"`
	EmbedFiles      []string      `json:"EmbedFiles"`
	TestGoFiles     []string      `json:"TestGoFiles"`
	XTestGoFiles    []string      `json:"XTestGoFiles"`
	TestEmbedFiles  []string      `json:"TestEmbedFiles"`
	XTestEmbedFiles []string      `json:"XTestEmbedFiles"`
}

func (pkg goListPackage) buildFiles(includeTests bool) []string {
	groups := [][]string{
		pkg.GoFiles, pkg.CgoFiles, pkg.CFiles, pkg.CXXFiles, pkg.MFiles, pkg.HFiles,
		pkg.FFiles, pkg.SFiles, pkg.SwigFiles, pkg.SwigCXXFiles, pkg.SysoFiles,
		pkg.EmbedFiles,
	}
	if includeTests {
		groups = append(groups, pkg.TestGoFiles, pkg.XTestGoFiles, pkg.TestEmbedFiles, pkg.XTestEmbedFiles)
	}
	var files []string
	for _, group := range groups {
		files = append(files, group...)
	}
	return files
}

type overlayReplacement struct {
	SourcePath string
	StubPath   string
	Package    string
}
type workloadOverlay struct {
	WorkloadID               string
	PackageImport            string
	PackageDirectory         string
	PackageName              string
	PerformanceTests         []string
	SuppressedTests          []string
	BenchmarkHarnessPackages []string
	Replacements             []overlayReplacement
	OverlayPath              string
	OverlayFileSHA256        string
}

func discoverWorkloadOverlay(
	ctx context.Context,
	runner CommandRunner,
	environment controlledGoEnvironment,
	repositoryRoot string,
	runtimeRoot string,
	workload Workload,
) (workloadOverlay, error) {
	moduleRoot := filepath.Join(repositoryRoot, filepath.FromSlash(workload.ModuleDir))
	result, err := runControlled(ctx, runner, Command{
		Executable:         environment.GoExecutable,
		Arguments:          []string{"list", "-e", "-mod=readonly", "-json", workload.Package},
		Directory:          moduleRoot,
		Environment:        environment.withWorkspace(filepath.Join(repositoryRoot, "go.work"), true),
		ReplaceEnvironment: true,
	})
	if err != nil {
		return workloadOverlay{}, fmt.Errorf("discover workload package %s: %w", workload.ID, err)
	}
	var target goListPackage
	if err := json.Unmarshal(commandStdout(result), &target); err != nil {
		return workloadOverlay{}, fmt.Errorf("decode workload package %s: %w", workload.ID, err)
	}
	if target.Dir == "" || target.ImportPath == "" || target.Name == "" {
		return workloadOverlay{}, fmt.Errorf("workload %s resolved an incomplete package", workload.ID)
	}
	harnessPackages, err := canonicalBenchmarkHarnessPackages(workload.BenchmarkHarnessPackages)
	if err != nil {
		return workloadOverlay{}, fmt.Errorf("workload %s benchmark harness packages: %w", workload.ID, err)
	}
	plan := workloadOverlay{
		WorkloadID: workload.ID, PackageImport: target.ImportPath,
		PackageDirectory: target.Dir, PackageName: target.Name,
		BenchmarkHarnessPackages: harnessPackages,
	}
	stubRoot := filepath.Join(runtimeRoot, "preliminary-overlays", workload.ID, "stubs")
	if err := os.MkdirAll(stubRoot, 0o700); err != nil {
		return workloadOverlay{}, fmt.Errorf("create preliminary overlay directory: %w", err)
	}
	active := append(append([]string(nil), target.TestGoFiles...), target.XTestGoFiles...)
	sort.Strings(active)
	for index, name := range active {
		source := filepath.Join(target.Dir, name)
		if strings.HasSuffix(name, performanceTestSuffix) {
			plan.PerformanceTests = append(plan.PerformanceTests, filepath.ToSlash(name))
			continue
		}
		packageName, err := sourcePackageName(source)
		if err != nil {
			return workloadOverlay{}, fmt.Errorf("read suppressed test %s: %w", source, err)
		}
		stub := filepath.Join(stubRoot, fmt.Sprintf("%03d-%s", index, filepath.Base(name)))
		if err := writeExclusive(stub, []byte("package "+packageName+"\n")); err != nil {
			return workloadOverlay{}, fmt.Errorf("write test overlay stub: %w", err)
		}
		plan.SuppressedTests = append(plan.SuppressedTests, filepath.ToSlash(name))
		plan.Replacements = append(plan.Replacements, overlayReplacement{
			SourcePath: source, StubPath: stub, Package: packageName,
		})
	}
	if len(plan.PerformanceTests) == 0 {
		return workloadOverlay{}, fmt.Errorf("workload %s package %s has no %s file", workload.ID, target.ImportPath, performanceTestSuffix)
	}
	overlayPath := filepath.Join(runtimeRoot, "preliminary-overlays", workload.ID, "overlay.json")
	digest, err := writeGoOverlay(overlayPath, plan.Replacements)
	if err != nil {
		return workloadOverlay{}, err
	}
	plan.OverlayPath = overlayPath
	plan.OverlayFileSHA256 = digest
	return plan, nil
}

func writeGoOverlay(path string, replacements []overlayReplacement) (string, error) {
	return writeGoOverlayRelativeTo(path, replacements, "")
}

func writeGoOverlayRelativeTo(path string, replacements []overlayReplacement, baseDirectory string) (string, error) {
	replace := make(map[string]string, len(replacements))
	for _, replacement := range replacements {
		source := replacement.SourcePath
		stub := replacement.StubPath
		var err error
		if baseDirectory == "" {
			source, err = filepath.Abs(source)
			if err != nil {
				return "", fmt.Errorf("resolve overlay source: %w", err)
			}
			stub, err = filepath.Abs(stub)
			if err != nil {
				return "", fmt.Errorf("resolve overlay stub: %w", err)
			}
		} else {
			source, err = filepath.Rel(baseDirectory, source)
			if err != nil || filepath.IsAbs(source) {
				return "", errors.Join(errors.New("make overlay source location-independent"), err)
			}
			stub, err = filepath.Rel(baseDirectory, stub)
			if err != nil || filepath.IsAbs(stub) {
				return "", errors.Join(errors.New("make overlay stub location-independent"), err)
			}
		}
		if _, duplicate := replace[source]; duplicate {
			return "", fmt.Errorf("overlay repeats source %s", source)
		}
		replace[source] = stub
	}
	encoded, err := json.Marshal(struct {
		Replace map[string]string `json:"Replace"`
	}{Replace: replace})
	if err != nil {
		return "", fmt.Errorf("encode Go overlay: %w", err)
	}
	if err := writeExclusive(path, encoded); err != nil {
		return "", fmt.Errorf("write Go overlay: %w", err)
	}
	return hashBytes(encoded), nil
}

func sourcePackageName(path string) (string, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly)
	if err != nil {
		return "", err
	}
	if parsed.Name == nil || parsed.Name.Name == "" {
		return "", errors.New("source has no package clause")
	}
	return parsed.Name.Name, nil
}

func canonicalBenchmarkHarnessPackages(packages []string) ([]string, error) {
	canonical := append([]string(nil), packages...)
	sort.Strings(canonical)
	for index, importPath := range canonical {
		if importPath == "" || importPath != strings.TrimSpace(importPath) ||
			path.Clean(importPath) != importPath || strings.Contains(importPath, "\\") ||
			strings.HasPrefix(importPath, ".") || strings.HasPrefix(importPath, "/") {
			return nil, fmt.Errorf("%q is not a canonical import path", importPath)
		}
		if index > 0 && canonical[index-1] == importPath {
			return nil, fmt.Errorf("%q is declared more than once", importPath)
		}
	}
	return canonical, nil
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
