package main

import (
	"encoding/json"
	"fmt"
	"go/build"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Source ownership is verified before publishing any diagnostic configuration,
// so pinned providers can never be exempted on the strength of a partial plan.
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
	profiles, err := readDiagnosticProfiles(root)
	if err != nil {
		return err
	}
	plan, err := planDiagnosticSources(root, files, build.Default, ports, profiles)
	if err != nil {
		return err
	}
	if len(plan) == 0 {
		return fmt.Errorf("maintained Go source set is empty")
	}
	// JSON preserves every path character and keeps configurations inseparable
	// from both the files that activate analysis and the diagnostic scope.
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
