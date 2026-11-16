package main

import (
	"encoding/json"
	"fmt"
	"go/build"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type diagnosticSession struct {
	Directory string   `json:"directory"`
	BuildTags []string `json:"build_tags,omitempty"`
	Files     []string `json:"files"`
}

// Owning modules and custom build-tag profiles are part of the diagnostic contract:
// each session uses its module graph and `-tags` configuration, while pinned providers
// remain verified dependencies.
func writeDiagnosticPlan(root string, output io.Writer) error {
	if err := verify(root, true); err != nil {
		return err
	}
	candidates, err := goSourceCandidates(root)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(root, "third_party", "pion", "manifest.json"))
	if err != nil {
		return err
	}
	var sources manifest
	if err = json.Unmarshal(raw, &sources); err != nil {
		return err
	}
	files, err := maintainedGoFiles(root, candidates, sources)
	if err != nil {
		return err
	}
	ports, err := goDistPorts()
	if err != nil {
		return err
	}
	plan, err := planDiagnosticSources(root, files, build.Default, ports)
	if err != nil {
		return err
	}
	if len(plan) == 0 {
		return fmt.Errorf("maintained Go source set is empty")
	}
	// Publish only a complete verified plan. JSON preserves every path character
	// and keeps session configuration inseparable from the files selected for analysis.
	return json.NewEncoder(output).Encode(plan)
}

func goSourceCandidates(root string) ([]string, error) {
	// Local validation precedes git add. Staging a new source must not change its
	// coverage, while ignored build and scratch artifacts must remain excluded.
	command := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "--deduplicate", "-z", "--", "*.go")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list working-tree Go sources: %w", err)
	}
	return strings.Split(string(output), "\x00"), nil
}

func maintainedGoFiles(root string, candidates []string, verified manifest) ([]string, error) {
	excluded := make(map[string]bool)
	for _, module := range verified.Modules {
		for _, file := range module.Files {
			excluded["third_party/pion/"+module.Name+"/"+file.Path] = true
		}
	}
	var files []string
	for _, file := range candidates {
		if file == "" || excluded[file] {
			continue
		}
		if !safeRelative(file) || !strings.HasSuffix(file, ".go") {
			return nil, fmt.Errorf("invalid Go source path: %q", file)
		}
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(file)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("Go source is not a regular file: %s", file)
		}
		files = append(files, file)
	}
	return files, nil
}

// Opening a foreign-platform file makes gopls retain another build view, and files
// requiring custom build tags need a `-tags` session so they are analyzed instead
// of orphaned. Within each session, finish host-native files before foreign views.
func planDiagnosticSources(
	root string,
	files []string,
	context build.Context,
	ports []goPort,
) ([]diagnosticSession, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	type source struct {
		path, module string
		tags         []string
		native       bool
	}
	sources := make([]source, 0, len(files))
	modules := make(map[string]string)
	for _, file := range files {
		path := filepath.Join(root, filepath.FromSlash(file))
		dir := filepath.Dir(path)
		module, err := diagnosticModule(root, dir, modules)
		if err != nil {
			return nil, err
		}
		tags, native, err := resolveDiagnosticSource(dir, filepath.Base(path), file, context, ports)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source{file, module, tags, native})
	}
	slices.SortFunc(sources, func(a, b source) int {
		if a.module != b.module {
			return strings.Compare(a.module, b.module)
		}
		if tagOrder := slices.Compare(a.tags, b.tags); tagOrder != 0 {
			return tagOrder
		}
		if a.native != b.native {
			if a.native {
				return -1
			}
			return 1
		}
		return strings.Compare(a.path, b.path)
	})
	var plan []diagnosticSession
	for _, source := range sources {
		directory, err := filepath.Rel(root, source.module)
		if err != nil {
			return nil, err
		}
		directory = filepath.ToSlash(directory)
		file, err := filepath.Rel(source.module, filepath.Join(root, filepath.FromSlash(source.path)))
		if err != nil {
			return nil, err
		}
		if len(plan) == 0 || plan[len(plan)-1].Directory != directory || !slices.Equal(plan[len(plan)-1].BuildTags, source.tags) {
			plan = append(plan, diagnosticSession{Directory: directory, BuildTags: source.tags})
		}
		group := &plan[len(plan)-1]
		group.Files = append(group.Files, filepath.ToSlash(file))
	}
	return plan, nil
}

func diagnosticModule(root, dir string, modules map[string]string) (string, error) {
	if module, ok := modules[dir]; ok {
		return module, nil
	}
	module := dir
	if dir != root {
		_, err := os.Stat(filepath.Join(dir, "go.mod"))
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		if os.IsNotExist(err) {
			module, err = diagnosticModule(root, filepath.Dir(dir), modules)
			if err != nil {
				return "", err
			}
		}
	}
	modules[dir] = module
	return module, nil
}
