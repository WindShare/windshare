package perfevidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/windshare/windshare/internal/perfevidence/processrun"
	"github.com/windshare/windshare/internal/processowner/protocol"
)

const (
	goAuthorityExecutableEnvironment = "WINDSHARE_GO_EXECUTABLE"
	publicGoProxy                    = "https://proxy.golang.org"
	publicGoSumDB                    = "sum.golang.org"
	goAssemblyIncludeRelativePath    = "pkg/include"
	maximumGoToolBinaries            = 256
)

type controlledGoEnvironment struct {
	GoExecutable       string
	Offline            []string
	Prefetch           []string
	Evidence           []EnvironmentVariable
	Effective          []EnvironmentVariable
	Toolchain          ToolchainIdentity
	ToolchainLocations ToolchainDiagnostics
	GoCache            string
	GoModCache         string
	Temporary          string
	AuthorityRoots     []string
	Authority          byteConsumptionAuthority
}

func prepareControlledGoEnvironment(
	ctx context.Context,
	runner CommandRunner,
	repositoryRoot string,
	runtimeRoot string,
) (result controlledGoEnvironment, resultErr error) {
	goExecutable := os.Getenv(goAuthorityExecutableEnvironment)
	var err error
	if goExecutable == "" {
		goExecutable, err = exec.LookPath("go")
		if err != nil {
			return controlledGoEnvironment{}, fmt.Errorf("locate Go toolchain: %w", err)
		}
	}
	goExecutable, err = filepath.Abs(goExecutable)
	if err != nil {
		return controlledGoEnvironment{}, fmt.Errorf("resolve Go executable: %w", err)
	}
	goExecutable, goRoot, toolchainAuthority, err := materializeGoToolchainGeneration(goExecutable, runtimeRoot)
	if err != nil {
		return controlledGoEnvironment{}, err
	}
	transferredAuthority := false
	defer func() {
		if !transferredAuthority {
			resultErr = errors.Join(resultErr, closeConsumptionAuthority(toolchainAuthority))
		}
	}()
	goCache := filepath.Join(runtimeRoot, "gocache")
	goModCache := filepath.Join(runtimeRoot, "gomodcache")
	temporary := filepath.Join(runtimeRoot, "tmp")
	for _, directory := range []string{goCache, goModCache, temporary} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return controlledGoEnvironment{}, fmt.Errorf("create controlled Go directory: %w", err)
		}
	}
	goWork := filepath.Join(repositoryRoot, "go.work")
	if _, err := os.Stat(goWork); err != nil {
		return controlledGoEnvironment{}, fmt.Errorf("inspect workspace file: %w", err)
	}
	controlled := map[string]string{
		"CGO_ENABLED":  "0",
		"GOCACHE":      goCache,
		"GODEBUG":      "",
		"GOENV":        "off",
		"GOEXPERIMENT": "",
		"GOFLAGS":      "",
		"GOGC":         "100",
		"GOMAXPROCS":   fmt.Sprintf("%d", runtime.NumCPU()),
		"GOMEMLIMIT":   "off",
		"GOMODCACHE":   goModCache,
		"GONOPROXY":    "",
		"GONOSUMDB":    "",
		"GOOS":         runtime.GOOS,
		"GOARCH":       runtime.GOARCH,
		"GOPRIVATE":    "",
		"GOPROXY":      "off",
		"GOSUMDB":      "off",
		"GOTOOLCHAIN":  "local",
		"GOVCS":        "*:off",
		"GOROOT":       goRoot,
		"GOWORK":       goWork,
		"TEMP":         temporary,
		"TMP":          temporary,
		"TMPDIR":       temporary,
	}
	offline := exactProcessEnvironment(controlled)
	prefetchValues := cloneStrings(controlled)
	prefetchValues["GOPROXY"] = publicGoProxy
	prefetchValues["GOSUMDB"] = publicGoSumDB
	prefetchValues["GOVCS"] = "public:git|hg,private:off"
	prefetch := exactProcessEnvironment(prefetchValues)
	version, err := runControlled(ctx, runner, Command{
		Executable: goExecutable, Arguments: []string{"version"}, Directory: repositoryRoot,
		Environment: offline, ReplaceEnvironment: true,
		authorities: []byteConsumptionAuthority{toolchainAuthority},
	})
	if err != nil {
		return controlledGoEnvironment{}, fmt.Errorf("identify Go toolchain: %w", err)
	}
	executableSHA, err := hashFile(goExecutable)
	if err != nil {
		return controlledGoEnvironment{}, fmt.Errorf("hash Go executable: %w", err)
	}
	effective, err := inspectEffectiveGoEnvironment(
		ctx, runner, goExecutable, repositoryRoot, offline, toolchainAuthority,
	)
	if err != nil {
		return controlledGoEnvironment{}, err
	}
	observedGoRoot := environmentValue(effective, "GOROOT")
	goVersion := environmentValue(effective, "GOVERSION")
	goToolDir := environmentValue(effective, "GOTOOLDIR")
	if observedGoRoot == "" || goVersion == "" || goToolDir == "" {
		return controlledGoEnvironment{}, errors.New("controlled Go environment omitted GOROOT, GOTOOLDIR, or GOVERSION")
	}
	if !samePath(observedGoRoot, goRoot) {
		return controlledGoEnvironment{}, fmt.Errorf(
			"materialized Go generation reported GOROOT %s, want %s", observedGoRoot, goRoot,
		)
	}
	toolBinaries, err := identifyToolBinaries(goToolDir)
	if err != nil {
		return controlledGoEnvironment{}, err
	}
	buildInputs, err := identifyToolchainInputsForRoots(goRoot, goToolDir, goAssemblyIncludeRelativePath)
	if err != nil {
		return controlledGoEnvironment{}, err
	}
	result = controlledGoEnvironment{
		GoExecutable: goExecutable,
		Offline:      offline,
		Prefetch:     prefetch,
		Evidence:     environmentVariables(controlled),
		Effective:    effective,
		Toolchain: ToolchainIdentity{
			ExecutableSHA256: executableSHA, Version: strings.TrimSpace(string(version.Output)),
			GoVersion: goVersion, Tools: toolBinaries, BuildInputs: buildInputs,
		},
		ToolchainLocations: ToolchainDiagnostics{
			ExecutablePath: goExecutable, GoRoot: goRoot, GoToolDir: goToolDir,
		},
		GoCache:        goCache,
		GoModCache:     goModCache,
		Temporary:      temporary,
		AuthorityRoots: []string{goRoot},
		Authority:      toolchainAuthority,
	}
	transferredAuthority = true
	return result, nil
}

// identifyToolchainInputsForRoots records the files the Go driver can consume
// while compiling a workload. The compiler reads assembly headers from
// GOROOT/pkg/include and executes every target-specific tool in GOTOOLDIR. A
// directory listing alone is not sufficient provenance: a changed header or a
// helper tool can produce a different binary while leaving the Go executable
// untouched. Keep logical paths relative to their semantic GOROOT or
// GOTOOLDIR authority so identity is independent of the installation layout.
func identifyToolchainInputsForRoots(goRoot, goToolDir, includeRelativeRoot string) ([]ToolchainInputIdentity, error) {
	roots, err := resolveToolchainInputRoots(goRoot, goToolDir, includeRelativeRoot)
	if err != nil {
		return nil, err
	}
	var inputs []ToolchainInputIdentity
	for _, root := range roots {
		rootInputs, err := inventoryToolchainInputRoot(root)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, rootInputs...)
	}
	if len(inputs) == 0 {
		return nil, errors.New("go toolchain build input directories are empty")
	}
	sort.Slice(inputs, func(left, right int) bool {
		if inputs[left].Root != inputs[right].Root {
			return inputs[left].Root < inputs[right].Root
		}
		return inputs[left].Path < inputs[right].Path
	})
	return inputs, nil
}

type toolchainInputRoot struct {
	kind       ToolchainInputRoot
	physical   string
	pathPrefix string
}

func resolveToolchainInputRoots(
	goRoot string,
	goToolDir string,
	includeRelativeRoot string,
) ([]toolchainInputRoot, error) {
	roots := []toolchainInputRoot{}
	if includeRelativeRoot != "" {
		root, err := confinedJoin(goRoot, includeRelativeRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve Go toolchain build inputs: %w", err)
		}
		roots = append(roots, toolchainInputRoot{
			kind: ToolchainInputGoRoot, physical: root, pathPrefix: filepath.ToSlash(includeRelativeRoot),
		})
	}
	if goToolDir != "" {
		// GOTOOLDIR is a separate semantic authority even when it happens to
		// live below GOROOT. Recording paths relative to that authority keeps
		// custom toolchains location-independent and makes final revalidation
		// resolve the same bytes rather than guessing from an installation layout.
		roots = append(roots, toolchainInputRoot{
			kind: ToolchainInputGoToolDir, physical: filepath.Clean(goToolDir),
		})
	}
	if len(roots) == 0 {
		return nil, errors.New("no Go toolchain input roots were provided")
	}
	return roots, nil
}

func inventoryToolchainInputRoot(root toolchainInputRoot) ([]ToolchainInputIdentity, error) {
	meter, err := newSnapshotInputMeter(defaultSnapshotInputBudget())
	if err != nil {
		return nil, err
	}
	var inputs []ToolchainInputIdentity
	err = walkBoundedTree(
		root.physical, maximumSnapshotInputObjects, maximumSnapshotInputDepth,
		func(path, relative string, info os.FileInfo) (bool, error) {
			if isReparsePointInfo(info) {
				return false, fmt.Errorf("go toolchain build input %s is a reparse point", path)
			}
			if info.IsDir() {
				return true, nil
			}
			if !info.Mode().IsRegular() {
				return false, fmt.Errorf("go toolchain build input %s is not a regular file", path)
			}
			if err := meter.observeBytes(relative, info.Size()); err != nil {
				return false, err
			}
			sha, err := hashFileExact(path, info.Size())
			if err != nil {
				return false, err
			}
			logical := filepath.ToSlash(relative)
			if root.pathPrefix != "" {
				logical = filepath.ToSlash(filepath.Join(filepath.FromSlash(root.pathPrefix), relative))
			}
			inputs = append(inputs, ToolchainInputIdentity{
				Root: root.kind, Path: logical, Bytes: info.Size(), SHA256: sha,
			})
			return false, nil
		})
	if err != nil {
		return nil, fmt.Errorf("inventory Go toolchain build inputs: %w", err)
	}
	return inputs, nil
}

func identifyToolBinaries(directory string) ([]ToolBinaryIdentity, error) {
	tools := make([]ToolBinaryIdentity, 0, maximumGoToolBinaries)
	err := walkBoundedTree(directory, maximumGoToolBinaries+1, 1, func(
		path, relative string, info os.FileInfo,
	) (bool, error) {
		if relative == "." {
			if !info.IsDir() || isReparsePointInfo(info) {
				return false, errors.New("go tool directory is not a real directory")
			}
			return true, nil
		}
		if isReparsePointInfo(info) || !info.Mode().IsRegular() {
			return false, fmt.Errorf("go tool %s is not a regular file", relative)
		}
		sha, err := hashFileExact(path, info.Size())
		if err != nil {
			return false, fmt.Errorf("hash Go tool %s: %w", relative, err)
		}
		tools = append(tools, ToolBinaryIdentity{Name: relative, Bytes: info.Size(), SHA256: sha})
		return false, nil
	})
	if err != nil {
		return nil, fmt.Errorf("inventory Go tool directory: %w", err)
	}
	if len(tools) == 0 {
		return nil, errors.New("go tool directory is empty")
	}
	sort.Slice(tools, func(left, right int) bool { return tools[left].Name < tools[right].Name })
	return tools, nil
}

func (environment controlledGoEnvironment) withWorkspace(goWork string, prefetch bool) []string {
	selected := environment.Offline
	if prefetch {
		selected = environment.Prefetch
	}
	return replaceEnvironmentValue(selected, "GOWORK", goWork)
}

func exactProcessEnvironment(controlled map[string]string) []string {
	allowed := map[string]struct{}{
		"APPDATA": {}, "COMSPEC": {}, "HOME": {}, "LOCALAPPDATA": {}, "PATH": {},
		"PATHEXT": {}, "SYSTEMDRIVE": {}, "SYSTEMROOT": {}, "USERPROFILE": {}, "WINDIR": {},
	}
	values := make(map[string]string, len(allowed)+len(controlled))
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		upper := strings.ToUpper(name)
		if _, keep := allowed[upper]; keep {
			values[upper] = value
		}
	}
	for name, value := range controlled {
		values[strings.ToUpper(name)] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result
}

func inspectEffectiveGoEnvironment(
	ctx context.Context,
	runner CommandRunner,
	goExecutable string,
	directory string,
	environment []string,
	authorities ...byteConsumptionAuthority,
) ([]EnvironmentVariable, error) {
	result, err := runControlled(ctx, runner, Command{
		Executable: goExecutable, Arguments: []string{"env", "-json"}, Directory: directory,
		Environment: environment, ReplaceEnvironment: true,
		authorities: authorities,
	})
	if err != nil {
		return nil, fmt.Errorf("inspect effective Go environment: %w", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(commandStdout(result), &decoded); err != nil {
		return nil, fmt.Errorf("decode effective Go environment: %w", err)
	}
	variables := make([]EnvironmentVariable, 0, len(decoded))
	for name, value := range decoded {
		encoded, encodeErr := json.Marshal(value)
		if encodeErr != nil {
			return nil, fmt.Errorf("encode Go environment %s: %w", name, encodeErr)
		}
		var canonical string
		if text, ok := value.(string); ok {
			canonical = text
		} else {
			canonical = string(encoded)
		}
		variables = append(variables, EnvironmentVariable{Name: name, Value: canonical})
	}
	sort.Slice(variables, func(left, right int) bool { return variables[left].Name < variables[right].Name })
	return variables, nil
}

func commandStdout(result CommandResult) []byte {
	if result.Stdout != nil {
		return result.Stdout
	}
	return result.Output
}

func runControlled(ctx context.Context, runner CommandRunner, command Command) (CommandResult, error) {
	if commandRunnerHasMutationDomain(runner) {
		command.mutationIntent = mutationIntentVerification
		command.restorePaths = true
	}
	result, err := runner.Run(ctx, command)
	if err != nil || result.ExitCode != 0 {
		return result, commandFailure(filepath.Base(command.Executable), result, err)
	}
	return result, nil
}

func commandRunnerHasMutationDomain(runner CommandRunner) bool {
	switch concrete := runner.(type) {
	case ProcessRunner:
		return concrete.MutationDomain != nil
	case *ProcessRunner:
		return concrete != nil && concrete.MutationDomain != nil
	default:
		return false
	}
}

func environmentVariables(values map[string]string) []EnvironmentVariable {
	result := make([]EnvironmentVariable, 0, len(values))
	for name, value := range values {
		result = append(result, EnvironmentVariable{Name: name, Value: value})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result
}

func environmentValue(values []EnvironmentVariable, name string) string {
	for _, value := range values {
		if value.Name == name {
			return value.Value
		}
	}
	return ""
}

func replaceEnvironmentValue(environment []string, name, value string) []string {
	upper := strings.ToUpper(name)
	result := append([]string(nil), environment...)
	for index, entry := range result {
		entryName, _, _ := strings.Cut(entry, "=")
		if strings.ToUpper(entryName) == upper {
			result[index] = upper + "=" + value
			return result
		}
	}
	return append(result, upper+"="+value)
}

func cloneStrings(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	maps.Copy(result, values)
	return result
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

func prepareOwnedCommand(command Command) (
	string,
	string,
	[]protocol.EnvironmentEntry,
	error,
) {
	executable, err := exec.LookPath(command.Executable)
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve owned command executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve owned command executable path: %w", err)
	}
	executable = filepath.Clean(executable)
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", nil, errors.Join(
			errors.New("owned command executable is not a regular file"),
			err,
		)
	}
	directory := command.Directory
	if directory == "" {
		directory, err = os.Getwd()
	} else {
		directory, err = filepath.Abs(directory)
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve owned command working directory: %w", err)
	}
	directory = filepath.Clean(directory)
	info, err = os.Stat(directory)
	if err != nil || !info.IsDir() {
		return "", "", nil, errors.Join(
			errors.New("owned command working directory is not a directory"),
			err,
		)
	}
	var base []string
	if !command.ReplaceEnvironment {
		base = os.Environ()
	}
	environment, err := processrun.CanonicalEnvironment(base, command.Environment)
	if err != nil {
		return "", "", nil, fmt.Errorf("canonicalize owned command environment: %w", err)
	}
	return executable, directory, environment, nil
}
