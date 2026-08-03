package perfevidence

import (
	"context"
	"crypto/sha1"
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

func CaptureSource(ctx context.Context, runner CommandRunner, repositoryRoot string) (SourceIdentity, error) {
	root, err := filepath.Abs(repositoryRoot)
	if err != nil {
		return SourceIdentity{}, fmt.Errorf("resolve repository root: %w", err)
	}
	commit, err := runGit(ctx, runner, root, "rev-parse", "HEAD")
	if err != nil {
		return SourceIdentity{}, err
	}
	status, err := runGit(ctx, runner, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return SourceIdentity{}, err
	}
	listed, err := runGit(ctx, runner, root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return SourceIdentity{}, err
	}
	paths, err := nulPaths(listed)
	if err != nil {
		return SourceIdentity{}, err
	}
	files := make([]SourceFile, 0, len(paths))
	for _, relative := range paths {
		record, recordErr := snapshotSourceFile(root, relative)
		if recordErr != nil {
			return SourceIdentity{}, recordErr
		}
		files = append(files, record)
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	identity := SourceIdentity{
		Commit:        strings.TrimSpace(string(commit)),
		WorktreeDirty: len(status) != 0,
		StatusSHA256:  hashBytes(status),
		Files:         files,
	}
	digestInput, err := json.Marshal(struct {
		Commit       string       `json:"commit"`
		StatusSHA256 string       `json:"statusSha256"`
		Files        []SourceFile `json:"files"`
	}{identity.Commit, identity.StatusSHA256, identity.Files})
	if err != nil {
		return SourceIdentity{}, fmt.Errorf("encode source identity: %w", err)
	}
	identity.SourceSHA256 = hashBytes(digestInput)
	return identity, nil
}

func SameSource(left, right SourceIdentity) bool {
	return left.Commit == right.Commit &&
		left.StatusSHA256 == right.StatusSHA256 &&
		left.SourceSHA256 == right.SourceSHA256
}

func requireStableSourceObservation(
	ctx context.Context,
	runner CommandRunner,
	repositoryRoot string,
	expected SourceIdentity,
	boundary string,
) error {
	observed, err := CaptureSource(ctx, runner, repositoryRoot)
	if err != nil {
		return fmt.Errorf("repeat source observation after %s: %w", boundary, err)
	}
	if !SameSource(expected, observed) {
		return fmt.Errorf(
			"source identity changed during %s (commit %s -> %s, status %s -> %s)",
			boundary, expected.Commit, observed.Commit, expected.StatusSHA256, observed.StatusSHA256,
		)
	}
	return nil
}

func runGit(ctx context.Context, runner CommandRunner, root string, arguments ...string) ([]byte, error) {
	result, err := runner.Run(ctx, Command{Executable: "git", Arguments: append([]string{"-C", root}, arguments...)})
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(result.Output)))
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("git %s exited with %d: %s", strings.Join(arguments, " "), result.ExitCode, strings.TrimSpace(string(result.Output)))
	}
	return result.Output, nil
}

func nulPaths(encoded []byte) ([]string, error) {
	if len(encoded) == 0 {
		return nil, nil
	}
	if encoded[len(encoded)-1] != 0 {
		return nil, errors.New("git path list was not NUL terminated")
	}
	parts := strings.Split(string(encoded[:len(encoded)-1]), "\x00")
	seen := make(map[string]struct{}, len(parts))
	for _, path := range parts {
		if path == "" {
			return nil, errors.New("git path list contained an empty path")
		}
		if _, duplicate := seen[path]; duplicate {
			return nil, fmt.Errorf("git path list repeated %q", path)
		}
		seen[path] = struct{}{}
	}
	return parts, nil
}

func snapshotSourceFile(root, relative string) (SourceFile, error) {
	canonical := filepath.ToSlash(relative)
	clean := filepath.Clean(filepath.FromSlash(canonical))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return SourceFile{}, fmt.Errorf("git returned unsafe source path %q", relative)
	}
	path := filepath.Join(root, clean)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return SourceFile{Path: canonical, Kind: "missing", Missing: true}, nil
	}
	if err != nil {
		return SourceFile{}, fmt.Errorf("inspect source file %s: %w", canonical, err)
	}
	record := SourceFile{Path: canonical, Mode: uint32(info.Mode()), Bytes: info.Size()}
	switch {
	case isReparsePointInfo(info):
		if info.Mode()&os.ModeSymlink == 0 {
			return SourceFile{}, fmt.Errorf("source path %s is an unsupported reparse point", canonical)
		}
		record.Kind = "symlink"
		var target string
		target, err = os.Readlink(path)
		record.Bytes = int64(len([]byte(target)))
		record.SHA256 = hashBytes([]byte(target))
	case info.Mode().IsRegular():
		record.Kind = "file"
		record.SHA256, err = hashFileExact(path, info.Size())
	case info.IsDir():
		record.Kind = "directory"
		record.Bytes = 0
		record.SHA256 = hashBytes(nil)
	default:
		return SourceFile{}, fmt.Errorf("source path %s has unsupported mode %s", canonical, info.Mode())
	}
	if err != nil {
		return SourceFile{}, fmt.Errorf("hash source file %s: %w", canonical, err)
	}
	return record, nil
}

func hashFile(path string) (digestValue string, resultErr error) {
	digestValue, _, resultErr = hashFileAtMost(path, maximumSnapshotSingleFileBytes)
	return digestValue, resultErr
}

func hashFileExact(path string, expectedBytes int64) (digestValue string, resultErr error) {
	if expectedBytes < 0 || expectedBytes > maximumSnapshotSingleFileBytes {
		return "", fmt.Errorf(
			"file %s byte count %d exceeds maximum %d",
			path, expectedBytes, maximumSnapshotSingleFileBytes,
		)
	}
	digestValue, observedBytes, err := hashFileAtMost(path, expectedBytes)
	if err != nil {
		return "", err
	}
	if observedBytes != expectedBytes {
		return "", fmt.Errorf("file %s has %d bytes, expected %d", path, observedBytes, expectedBytes)
	}
	return digestValue, nil
}

func hashFileAtMost(path string, maximumBytes int64) (digestValue string, observedBytes int64, resultErr error) {
	if maximumBytes < 0 || maximumBytes >= int64(^uint64(0)>>1) {
		return "", 0, fmt.Errorf("file %s has an invalid hash byte limit %d", path, maximumBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() || isReparsePointInfo(info) {
		return "", 0, fmt.Errorf("file %s is not a real regular file", path)
	}
	if info.Size() > maximumBytes {
		return "", 0, fmt.Errorf("file %s exceeds maximum byte count %d", path, maximumBytes)
	}
	digest := sha256.New()
	// FileInfo is only a preflight observation. Limiting the reader makes the
	// declared byte budget remain authoritative if a concurrent writer grows
	// the file before or during hashing.
	observedBytes, err = io.Copy(digest, io.LimitReader(file, maximumBytes+1))
	if err != nil {
		return "", observedBytes, err
	}
	if observedBytes > maximumBytes {
		return "", observedBytes, fmt.Errorf("file %s exceeded maximum byte count %d while hashing", path, maximumBytes)
	}
	return hex.EncodeToString(digest.Sum(nil)), observedBytes, nil
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
