package perfevidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
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
	Dir        string        `json:"Dir"`
	ImportPath string        `json:"ImportPath"`
	Name       string        `json:"Name"`
	ForTest    string        `json:"ForTest"`
	Standard   bool          `json:"Standard"`
	Module     *goListModule `json:"Module"`
	Deps       []string      `json:"Deps"`

	GoFiles         []string `json:"GoFiles"`
	CgoFiles        []string `json:"CgoFiles"`
	CFiles          []string `json:"CFiles"`
	CXXFiles        []string `json:"CXXFiles"`
	MFiles          []string `json:"MFiles"`
	HFiles          []string `json:"HFiles"`
	FFiles          []string `json:"FFiles"`
	SFiles          []string `json:"SFiles"`
	SwigFiles       []string `json:"SwigFiles"`
	SwigCXXFiles    []string `json:"SwigCXXFiles"`
	SysoFiles       []string `json:"SysoFiles"`
	EmbedFiles      []string `json:"EmbedFiles"`
	TestGoFiles     []string `json:"TestGoFiles"`
	XTestGoFiles    []string `json:"XTestGoFiles"`
	TestEmbedFiles  []string `json:"TestEmbedFiles"`
	XTestEmbedFiles []string `json:"XTestEmbedFiles"`
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
