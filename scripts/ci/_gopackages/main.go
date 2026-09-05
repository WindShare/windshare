package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

const (
	allSetName     = "all"
	coreSetName    = "core"
	nonCoreSetName = "non-core"

	allPattern  = "./..."
	corePattern = "./core/..."

	repositoryModulePath  = "github.com/windshare/windshare"
	retiredCoreModulePath = repositoryModulePath + "/core"
	rootGoModPath         = "go.mod"
)

// Go package wildcards intentionally skip nested modules, so package ownership
// is meaningful only after every module boundary has an explicit validation owner.
var approvedModuleMetadataPaths = map[string]struct{}{
	"go.mod":                       {},
	"go.sum":                       {},
	"internal/perfevidence/go.mod": {},
	"internal/perfevidence/go.sum": {},
	"spikes/webrtc/go.mod":         {},
	"spikes/webrtc/go.sum":         {},
	// Pinned upstream dependency projections are owned by _piondeps and its
	// reproducible patch gate; WindShare wrappers remain in the root sets.
	"third_party/pion/ice/go.mod":    {},
	"third_party/pion/ice/go.sum":    {},
	"third_party/pion/webrtc/go.mod": {},
	"third_party/pion/webrtc/go.sum": {},
}

var moduleMetadataPathspecs = []string{
	"go.mod",
	"go.sum",
	"go.work",
	"go.work.sum",
	":(glob)**/go.mod",
	":(glob)**/go.sum",
	":(glob)**/go.work",
	":(glob)**/go.work.sum",
}

type packageSets struct {
	all     []string
	core    []string
	nonCore []string
}

type packageLister interface {
	list(context.Context, string) ([]string, error)
}

type moduleLayoutValidator interface {
	validate(context.Context) error
}

type goPackageLister struct {
	directory string
}

type repositoryModuleLayout struct {
	directory string
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "gopackages: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("gopackages", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	requestedSet := flags.String("set", "", "package set to emit: all, core, or non-core")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}

	sets, err := loadPackageSets(
		ctx,
		repositoryModuleLayout{directory: "."},
		goPackageLister{directory: "."},
	)
	if err != nil {
		return err
	}

	var packages []string
	switch *requestedSet {
	case allSetName:
		packages = sets.all
	case coreSetName:
		packages = sets.core
	case nonCoreSetName:
		packages = sets.nonCore
	default:
		return fmt.Errorf("-set must be one of %q, %q, or %q", allSetName, coreSetName, nonCoreSetName)
	}

	writer := bufio.NewWriter(output)
	for _, packagePath := range packages {
		if _, err := fmt.Fprintln(writer, packagePath); err != nil {
			return fmt.Errorf("write package set: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush package set: %w", err)
	}
	return nil
}

func loadPackageSets(
	ctx context.Context,
	layout moduleLayoutValidator,
	lister packageLister,
) (packageSets, error) {
	if err := layout.validate(ctx); err != nil {
		return packageSets{}, fmt.Errorf("validate repository module layout: %w", err)
	}

	all, err := lister.list(ctx, allPattern)
	if err != nil {
		return packageSets{}, fmt.Errorf("list production packages: %w", err)
	}
	core, err := lister.list(ctx, corePattern)
	if err != nil {
		return packageSets{}, fmt.Errorf("list core packages: %w", err)
	}
	return derivePackageSets(all, core)
}

func (layout repositoryModuleLayout) validate(ctx context.Context) error {
	metadataPaths, err := layout.listMetadataPaths(ctx)
	if err != nil {
		return err
	}
	rootGoMod, err := os.ReadFile(filepath.Join(layout.directory, rootGoModPath))
	if err != nil {
		return fmt.Errorf("read %s: %w", rootGoModPath, err)
	}
	return validateModuleLayout(metadataPaths, rootGoMod)
}

func (layout repositoryModuleLayout) listMetadataPaths(ctx context.Context) ([]string, error) {
	arguments := []string{"-c", "core.quotepath=false", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--"}
	arguments = append(arguments, moduleMetadataPathspecs...)
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = layout.directory

	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return nil, fmt.Errorf("list repository module metadata: %w", err)
		}
		return nil, fmt.Errorf("list repository module metadata: %w: %s", err, detail)
	}

	seen := make(map[string]struct{})
	for _, rawPath := range bytes.Split(output, []byte{0}) {
		if len(rawPath) == 0 {
			continue
		}
		repositoryPath := normalizeRepositoryPath(string(rawPath))
		_, statErr := os.Stat(filepath.Join(layout.directory, filepath.FromSlash(repositoryPath)))
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, fmt.Errorf("inspect module metadata %s: %w", repositoryPath, statErr)
		}
		seen[repositoryPath] = struct{}{}
	}
	metadataPaths := make([]string, 0, len(seen))
	for repositoryPath := range seen {
		metadataPaths = append(metadataPaths, repositoryPath)
	}
	sort.Strings(metadataPaths)
	return metadataPaths, nil
}

func validateModuleLayout(metadataPaths []string, rootGoMod []byte) error {
	rootGoModObserved := false
	for _, metadataPath := range metadataPaths {
		normalizedPath := normalizeRepositoryPath(metadataPath)
		if !isModuleMetadataPath(normalizedPath) {
			continue
		}
		if _, approved := approvedModuleMetadataPaths[normalizedPath]; !approved {
			return fmt.Errorf("unapproved module or workspace metadata %q", normalizedPath)
		}
		if normalizedPath == rootGoModPath {
			rootGoModObserved = true
		}
	}
	if !rootGoModObserved {
		return fmt.Errorf("repository metadata does not include %s", rootGoModPath)
	}

	parsed, err := modfile.Parse(rootGoModPath, rootGoMod, nil)
	if err != nil {
		return fmt.Errorf("parse %s: %w", rootGoModPath, err)
	}
	if parsed.Module == nil || parsed.Module.Mod.Path != repositoryModulePath {
		return fmt.Errorf("%s must declare module %s", rootGoModPath, repositoryModulePath)
	}
	for _, requirement := range parsed.Require {
		if requirement.Mod.Path == retiredCoreModulePath {
			return fmt.Errorf("%s retains retired core module requirement %s", rootGoModPath, retiredCoreModulePath)
		}
	}
	return nil
}

func normalizeRepositoryPath(repositoryPath string) string {
	portablePath := strings.ReplaceAll(repositoryPath, "\\", "/")
	return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(portablePath)), "./")
}

func isModuleMetadataPath(repositoryPath string) bool {
	name := filepath.Base(repositoryPath)
	return name == "go.mod" || name == "go.sum" || name == "go.work" || name == "go.work.sum"
}

func (lister goPackageLister) list(ctx context.Context, pattern string) ([]string, error) {
	command := exec.CommandContext(ctx, "go", "list", "-f={{.ImportPath}}", pattern)
	command.Dir = lister.directory
	command.Env = environmentWithGOWORKOff(os.Environ())

	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", err, detail)
	}

	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, fmt.Errorf("%s matched no packages", pattern)
	}
	return strings.Fields(trimmed), nil
}

func environmentWithGOWORKOff(environment []string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(key, "GOWORK") {
			continue
		}
		result = append(result, entry)
	}
	return append(result, "GOWORK=off")
}

func derivePackageSets(all, core []string) (packageSets, error) {
	allPackages, allMembership, err := uniqueSorted(allSetName, all)
	if err != nil {
		return packageSets{}, err
	}
	corePackages, coreMembership, err := uniqueSorted(coreSetName, core)
	if err != nil {
		return packageSets{}, err
	}
	if len(allPackages) == 0 {
		return packageSets{}, errors.New("all package set is empty")
	}
	if len(corePackages) == 0 {
		return packageSets{}, errors.New("core package set is empty")
	}

	for packagePath := range coreMembership {
		if _, ok := allMembership[packagePath]; !ok {
			return packageSets{}, fmt.Errorf("core package %q is absent from the production package universe", packagePath)
		}
	}

	nonCorePackages := make([]string, 0, len(allPackages)-len(corePackages))
	for _, packagePath := range allPackages {
		if _, isCore := coreMembership[packagePath]; !isCore {
			nonCorePackages = append(nonCorePackages, packagePath)
		}
	}
	if len(nonCorePackages) == 0 {
		return packageSets{}, errors.New("non-core package set is empty")
	}

	sets := packageSets{
		all:     allPackages,
		core:    corePackages,
		nonCore: nonCorePackages,
	}
	if err := sets.validateOwnership(); err != nil {
		return packageSets{}, err
	}
	return sets, nil
}

func uniqueSorted(name string, packages []string) ([]string, map[string]struct{}, error) {
	sortedPackages := append([]string(nil), packages...)
	sort.Strings(sortedPackages)
	membership := make(map[string]struct{}, len(sortedPackages))
	for _, packagePath := range sortedPackages {
		if packagePath == "" {
			return nil, nil, fmt.Errorf("%s package set contains an empty import path", name)
		}
		if _, exists := membership[packagePath]; exists {
			return nil, nil, fmt.Errorf("%s package set contains duplicate %q", name, packagePath)
		}
		membership[packagePath] = struct{}{}
	}
	return sortedPackages, membership, nil
}

func (sets packageSets) validateOwnership() error {
	allMembership := make(map[string]struct{}, len(sets.all))
	for _, packagePath := range sets.all {
		allMembership[packagePath] = struct{}{}
	}

	owners := make(map[string]string, len(sets.all))
	for _, packagePath := range sets.core {
		owners[packagePath] = coreSetName
	}
	for _, packagePath := range sets.nonCore {
		if owner, exists := owners[packagePath]; exists {
			return fmt.Errorf("package %q belongs to both %s and %s sets", packagePath, owner, nonCoreSetName)
		}
		owners[packagePath] = nonCoreSetName
	}

	for packagePath := range allMembership {
		if _, owned := owners[packagePath]; !owned {
			return fmt.Errorf("production package %q has no validation owner", packagePath)
		}
	}
	for packagePath, owner := range owners {
		if _, exists := allMembership[packagePath]; !exists {
			return fmt.Errorf("%s package %q is outside the production package universe", owner, packagePath)
		}
	}
	return nil
}
