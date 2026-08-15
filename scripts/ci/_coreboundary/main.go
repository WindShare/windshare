package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

const (
	repositoryModulePath = "github.com/windshare/windshare"
	coreImportPath       = repositoryModulePath + "/core"
	corePackagePattern   = "./core/..."
	boundaryGOARCH       = "amd64"
)

var supportedTargets = []goTarget{
	{GOOS: "linux", GOARCH: boundaryGOARCH},
	{GOOS: "windows", GOARCH: boundaryGOARCH},
	{GOOS: "darwin", GOARCH: boundaryGOARCH},
}

var allowedThirdPartyModules = map[string]struct{}{
	"github.com/fxamacker/cbor/v2": {},
	"github.com/x448/float16":      {},
	"golang.org/x/sys":             {},
	"golang.org/x/text":            {},
}

var prohibitedPackageCapabilities = []pathFamily{
	{Root: "crypto/tls", Capability: "TLS transport"},
	{Root: "net/http", Capability: "HTTP transport"},
	{Root: "net/rpc", Capability: "RPC transport"},
	{Root: "net/smtp", Capability: "SMTP transport"},
}

var prohibitedModuleCapabilities = []pathFamily{
	{Root: "github.com/coder/websocket", Capability: "WebSocket transport"},
	{Root: "github.com/gorilla/websocket", Capability: "WebSocket transport"},
	{Root: "github.com/pion", Capability: "Pion concrete transport"},
	{Root: "nhooyr.io/websocket", Capability: "WebSocket transport"},
}

// These capabilities certify native behavior in tests. Keeping the exceptions
// on the graph delta prevents a test import from silently authorizing production.
var nativeTestOnlyCapabilities = map[string]testCapability{
	"net": {
		Reason:                    "raw network sockets",
		AllowProductionTransitive: true,
	},
	"os/exec": {
		Reason: "child-process execution",
	},
}

type goTarget struct {
	GOOS   string
	GOARCH string
}

func (t goTarget) String() string {
	return t.GOOS + "/" + t.GOARCH
}

type graphScope string

const (
	productionScope graphScope = "production"
	testDeltaScope  graphScope = "test-delta"
)

type listedModule struct {
	Path    string
	Replace *listedModule
}

type listedPackage struct {
	ImportPath string
	ForTest    string
	Imports    []string
	Standard   bool
	Module     *listedModule
}

type pathFamily struct {
	Root       string
	Capability string
}

type testCapability struct {
	Reason                    string
	AllowProductionTransitive bool
}

type policyFinding struct {
	Target     goTarget
	Scope      graphScope
	ImportPath string
	Reason     string
}

func (f policyFinding) String() string {
	return fmt.Sprintf(
		"target=%s scope=%s package=%s: %s",
		f.Target,
		f.Scope,
		f.ImportPath,
		f.Reason,
	)
}

type graphLister interface {
	List(context.Context, goTarget, bool) ([]listedPackage, error)
}

type commandGraphLister struct {
	GoExecutable     string
	GoArgumentPrefix []string
	RepositoryRoot   string
	BaseEnv          []string
}

func (l commandGraphLister) List(
	ctx context.Context,
	target goTarget,
	includeTests bool,
) ([]listedPackage, error) {
	arguments := append([]string(nil), l.GoArgumentPrefix...)
	arguments = append(arguments, "list", "-deps", "-json")
	if includeTests {
		arguments = append(arguments, "-test")
	}
	arguments = append(arguments, corePackagePattern)

	command := exec.CommandContext(ctx, l.GoExecutable, arguments...)
	command.Dir = l.RepositoryRoot
	command.Env = targetEnvironment(l.BaseEnv, target)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("go list failed: %s", detail)
	}

	packages, err := decodePackageStream(&stdout)
	if err != nil {
		return nil, fmt.Errorf("decode go list output: %w", err)
	}
	return packages, nil
}

func main() {
	repositoryRoot, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-boundary: determine repository root: %v\n", err)
		os.Exit(1)
	}
	goExecutable := os.Getenv("WINDSHARE_GO_EXECUTABLE")
	if goExecutable == "" {
		goExecutable = "go"
	}

	lister := commandGraphLister{
		GoExecutable:   goExecutable,
		RepositoryRoot: repositoryRoot,
		BaseEnv:        os.Environ(),
	}
	findings := runBoundary(context.Background(), lister, os.Stdout)
	if len(findings) != 0 {
		for _, finding := range findings {
			fmt.Fprintf(os.Stderr, "core-boundary: %s\n", finding)
		}
		os.Exit(1)
	}

	fmt.Fprintf(
		os.Stdout,
		"core-boundary: PASS targets=%s third_party_modules=%s\n",
		strings.Join(targetNames(supportedTargets), ","),
		strings.Join(sortedKeys(allowedThirdPartyModules), ","),
	)
}

func runBoundary(
	ctx context.Context,
	lister graphLister,
	progress io.Writer,
) []policyFinding {
	var findings []policyFinding
	for _, target := range supportedTargets {
		productionPackages, productionErr := lister.List(ctx, target, false)
		if productionErr != nil {
			findings = append(findings, policyFinding{
				Target:     target,
				Scope:      productionScope,
				ImportPath: corePackagePattern,
				Reason:     productionErr.Error(),
			})
		}

		testPackages, testErr := lister.List(ctx, target, true)
		if testErr != nil {
			findings = append(findings, policyFinding{
				Target:     target,
				Scope:      testDeltaScope,
				ImportPath: corePackagePattern,
				Reason:     testErr.Error(),
			})
		}

		if productionErr != nil || testErr != nil {
			continue
		}

		testDelta := packageDifference(testPackages, productionPackages)
		findings = append(findings, validatePackages(target, productionScope, productionPackages)...)
		findings = append(findings, validatePackages(target, testDeltaScope, testDelta)...)
		fmt.Fprintf(
			progress,
			"core-boundary: target=%s production_packages=%d test_delta_packages=%d native_test_capabilities=%s\n",
			target,
			len(productionPackages),
			len(testDelta),
			strings.Join(observedTestCapabilities(testDelta), ","),
		)
	}

	sort.Slice(findings, func(i, j int) bool {
		return findings[i].String() < findings[j].String()
	})
	return findings
}

func validatePackages(
	target goTarget,
	scope graphScope,
	packages []listedPackage,
) []policyFinding {
	var findings []policyFinding
	for _, pkg := range packages {
		for _, reason := range packagePolicyViolations(scope, pkg) {
			findings = append(findings, policyFinding{
				Target:     target,
				Scope:      scope,
				ImportPath: pkg.ImportPath,
				Reason:     reason,
			})
		}
	}
	return findings
}

func packagePolicyViolations(scope graphScope, pkg listedPackage) []string {
	logicalImportPath := pkg.ImportPath
	if pkg.ForTest != "" {
		logicalImportPath = pkg.ForTest
	}

	if isWindShareImportPath(logicalImportPath) {
		if !isCoreImportPath(logicalImportPath) {
			return []string{"WindShare package is outside the core boundary"}
		}
		if pkg.Module == nil || pkg.Module.Path == "" {
			return []string{"core package has no auditable repository module identity"}
		}
		if pkg.Module.Path != repositoryModulePath {
			return []string{fmt.Sprintf(
				"core import path is owned by module %s, want %s",
				pkg.Module.Path,
				repositoryModulePath,
			)}
		}
		return coreDirectImportViolations(scope, pkg.Imports)
	}

	if family, ok := matchingFamily(pkg.ImportPath, prohibitedPackageCapabilities); ok {
		return []string{fmt.Sprintf("prohibited %s capability", family.Capability)}
	}

	if capability, ok := nativeTestOnlyCapabilities[pkg.ImportPath]; ok {
		if scope == testDeltaScope || capability.AllowProductionTransitive {
			return nil
		}
		return []string{fmt.Sprintf(
			"native test-only capability reached the production graph (%s)",
			capability.Reason,
		)}
	}

	if pkg.Standard {
		return nil
	}
	if pkg.Module == nil || pkg.Module.Path == "" {
		return []string{"external package has no auditable module identity"}
	}
	if family, ok := matchingFamily(pkg.Module.Path, prohibitedModuleCapabilities); ok {
		return []string{fmt.Sprintf(
			"prohibited %s module %s",
			family.Capability,
			pkg.Module.Path,
		)}
	}
	if _, ok := allowedThirdPartyModules[pkg.Module.Path]; !ok {
		return []string{fmt.Sprintf("third-party module %s is not allowlisted", pkg.Module.Path)}
	}

	if pkg.Module.Replace != nil {
		replacementPath := pkg.Module.Replace.Path
		if _, ok := allowedThirdPartyModules[replacementPath]; !ok {
			return []string{fmt.Sprintf(
				"allowlisted module %s is replaced by unreviewed source %s",
				pkg.Module.Path,
				replacementPath,
			)}
		}
	}
	return nil
}

func coreDirectImportViolations(scope graphScope, imports []string) []string {
	var violations []string
	for _, importedPath := range imports {
		if family, ok := matchingFamily(importedPath, prohibitedPackageCapabilities); ok {
			violations = append(violations, fmt.Sprintf(
				"directly imports prohibited %s capability %s",
				family.Capability,
				importedPath,
			))
			continue
		}
		capability, ok := nativeTestOnlyCapabilities[importedPath]
		if ok && scope == productionScope && capability.AllowProductionTransitive {
			violations = append(violations, fmt.Sprintf(
				"directly imports native test-only capability %s (%s)",
				importedPath,
				capability.Reason,
			))
		}
	}
	return violations
}

func isWindShareImportPath(importPath string) bool {
	return inPathFamily(importPath, repositoryModulePath)
}

func isCoreImportPath(importPath string) bool {
	return inPathFamily(importPath, coreImportPath)
}

func matchingFamily(importPath string, families []pathFamily) (pathFamily, bool) {
	for _, family := range families {
		if inPathFamily(importPath, family.Root) {
			return family, true
		}
	}
	return pathFamily{}, false
}

func inPathFamily(importPath, root string) bool {
	return importPath == root || strings.HasPrefix(importPath, root+"/")
}

func packageDifference(all, baseline []listedPackage) []listedPackage {
	baselinePaths := make(map[string]struct{}, len(baseline))
	for _, pkg := range baseline {
		baselinePaths[pkg.ImportPath] = struct{}{}
	}

	delta := make([]listedPackage, 0, len(all))
	for _, pkg := range all {
		if _, exists := baselinePaths[pkg.ImportPath]; !exists {
			delta = append(delta, pkg)
		}
	}
	return delta
}

func observedTestCapabilities(packages []listedPackage) []string {
	observed := make(map[string]struct{})
	for _, pkg := range packages {
		if _, ok := nativeTestOnlyCapabilities[pkg.ImportPath]; ok {
			observed[pkg.ImportPath] = struct{}{}
		}
	}
	return sortedKeys(observed)
}

func decodePackageStream(input io.Reader) ([]listedPackage, error) {
	decoder := json.NewDecoder(input)
	var packages []listedPackage
	for {
		var pkg listedPackage
		err := decoder.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if pkg.ImportPath == "" {
			return nil, errors.New("package is missing ImportPath")
		}
		packages = append(packages, pkg)
	}
	return packages, nil
}

func targetEnvironment(base []string, target goTarget) []string {
	blocked := map[string]struct{}{
		"CGO_ENABLED": {},
		"GOARCH":      {},
		"GOOS":        {},
		"GOWORK":      {},
	}
	environment := make([]string, 0, len(base)+len(blocked))
	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, found := blocked[strings.ToUpper(name)]; !found {
			environment = append(environment, entry)
		}
	}
	return append(
		environment,
		"CGO_ENABLED=0",
		"GOARCH="+target.GOARCH,
		"GOOS="+target.GOOS,
		"GOWORK=off",
	)
}

func targetNames(targets []goTarget) []string {
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.String())
	}
	return names
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
