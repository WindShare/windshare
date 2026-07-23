// Command d5networkpolicy proves the compiler-visible test resource boundary.
// Direct test-owned primitives and VTA-resolved function, method, interface,
// closure, phi, and container flows must cross a runtime gate. Arbitrary static
// production business calls are deliberately outside this value-flow proof;
// their real-network tests are selected by an exact compiler-derived execution
// plan and authorized at runtime by the parent-owned fixed-path capability.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	networkManifestSchemaVersion = 3
)

var manifestPackageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type packageRecord struct {
	Name string `json:"Name"`
	Path string `json:"Path"`
}

type manifestDocument struct {
	SchemaVersion int             `json:"SchemaVersion"`
	Packages      []packageRecord `json:"Packages"`
}

type fixedManifest struct {
	packages map[string]bool
}

type analysisResult struct {
	classified map[string]bool
	violations []string
	catalog    semanticCatalog
}

func main() {
	rootFlag := flag.String("root", ".", "repository root")
	manifestFlag := flag.String("manifest", "scripts/d5-windows-network-packages.json", "fixed package manifest")
	planRequestFlag := flag.String("execution-plan-request", "", "exact test execution-plan request JSON")
	planOutputFlag := flag.String("execution-plan-output", "", "compiler-derived execution-plan JSON")
	flag.Parse()
	root, err := filepath.Abs(*rootFlag)
	if err != nil {
		fatalf("resolve repository root: %v", err)
	}
	manifest, err := loadManifest(resolveRootPath(root, *manifestFlag))
	if err != nil {
		fatalf("load fixed package manifest: %v", err)
	}
	result, err := analyzeRoot(root)
	if err != nil {
		fatalf("analyze test resource ownership: %v", err)
	}
	for path := range result.classified {
		if !manifest.packages[path] {
			result.violations = append(result.violations,
				"network/process test package is absent from the fixed runner manifest: "+path)
		}
	}
	for path := range manifest.packages {
		if !result.classified[path] {
			result.violations = append(result.violations,
				"fixed runner manifest package has no semantic network/process ownership: "+path)
		}
	}
	sort.Strings(result.violations)
	if len(result.violations) != 0 {
		for _, violation := range result.violations {
			fmt.Fprintln(os.Stderr, violation)
		}
		os.Exit(1)
	}
	if (*planRequestFlag == "") != (*planOutputFlag == "") {
		fatalf("-execution-plan-request and -execution-plan-output must be supplied together")
	}
	if *planRequestFlag != "" {
		request, err := loadExecutionPlanRequest(resolveRootPath(root, *planRequestFlag))
		if err != nil {
			fatalf("load execution-plan request: %v", err)
		}
		document, err := buildExecutionPlanDocument(
			root,
			manifest.packages,
			result.catalog,
			request,
		)
		if err != nil {
			fatalf("construct execution plan: %v", err)
		}
		if err := writeExecutionPlanDocument(resolveRootPath(root, *planOutputFlag), document); err != nil {
			fatalf("write execution plan: %v", err)
		}
		fmt.Printf(
			"D5 Windows semantic network boundary PASS: %d exact packages; %d bound execution plans\n",
			len(result.classified),
			len(document.Plans),
		)
		return
	}
	fmt.Printf("D5 Windows semantic network boundary PASS: %d exact packages\n", len(result.classified))
}

func resolveRootPath(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, filepath.FromSlash(path))
}

func loadManifest(path string) (fixedManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fixedManifest{}, err
	}
	var document manifestDocument
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return fixedManifest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixedManifest{}, errors.New("fixed package manifest contains trailing JSON")
	}
	if document.SchemaVersion != networkManifestSchemaVersion {
		return fixedManifest{}, fmt.Errorf(
			"fixed package manifest schema = %d, want %d",
			document.SchemaVersion,
			networkManifestSchemaVersion,
		)
	}
	packages := make(map[string]bool, len(document.Packages))
	names := make(map[string]bool, len(document.Packages))
	for _, record := range document.Packages {
		path := strings.TrimPrefix(filepath.ToSlash(record.Path), "./")
		if !manifestPackageNamePattern.MatchString(record.Name) ||
			path == "" || path == "." || strings.HasPrefix(path, "../") ||
			packages[path] || names[record.Name] {
			return fixedManifest{}, fmt.Errorf("invalid or duplicate package record: %+v", record)
		}
		packages[path] = true
		names[record.Name] = true
	}
	if len(packages) == 0 {
		return fixedManifest{}, errors.New("fixed package manifest has no active packages")
	}
	return fixedManifest{packages: packages}, nil
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
