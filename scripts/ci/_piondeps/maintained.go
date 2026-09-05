package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gopls must open maintained files in their owning modules. Opening a pinned
// dependency as a workspace file instead selects its standalone upstream graph,
// losing the root module's provider replacements and inferring unsupported JS views.
func writeMaintainedGoFiles(root string, nul bool, output io.Writer) error {
	if err := verify(root, true); err != nil {
		return err
	}
	command := exec.Command("git", "ls-files", "-z", "--", "*.go")
	command.Dir = root
	tracked, err := command.Output()
	if err != nil {
		return fmt.Errorf("list tracked Go sources: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "third_party", "pion", "manifest.json"))
	if err != nil {
		return err
	}
	var sources manifest
	if err = json.Unmarshal(raw, &sources); err != nil {
		return err
	}
	files, err := maintainedGoFiles(root, strings.Split(string(tracked), "\x00"), sources)
	if err != nil {
		return err
	}
	separator := "\n"
	if nul {
		separator = "\x00"
	}
	// Validate the complete list before writing so a failed selector never feeds
	// a partial analysis set to either platform launcher.
	for _, file := range files {
		if !nul && strings.ContainsAny(file, "\r\n") {
			return fmt.Errorf("Go source path requires NUL output: %q", file)
		}
	}
	for _, file := range files {
		if _, err = io.WriteString(output, file+separator); err != nil {
			return err
		}
	}
	return nil
}

func maintainedGoFiles(root string, tracked []string, verified manifest) ([]string, error) {
	excluded := make(map[string]bool)
	for _, module := range verified.Modules {
		for _, file := range module.Files {
			excluded["third_party/pion/"+module.Name+"/"+file.Path] = true
		}
	}
	var files []string
	for _, file := range tracked {
		if file == "" || excluded[file] {
			continue
		}
		if !safeRelative(file) || !strings.HasSuffix(file, ".go") {
			return nil, fmt.Errorf("invalid tracked Go source path: %q", file)
		}
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(file)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("tracked Go source is not a regular file: %s", file)
		}
		files = append(files, file)
	}
	return files, nil
}
